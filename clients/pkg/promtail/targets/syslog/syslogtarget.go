package syslog

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/backoff"
	"github.com/influxdata/go-syslog/v3"
	"github.com/influxdata/go-syslog/v3/rfc5424"
	"github.com/mwitkow/go-conntrack"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/relabel"

	"github.com/grafana/loki/clients/pkg/promtail/api"
	"github.com/grafana/loki/clients/pkg/promtail/scrapeconfig"
	"github.com/grafana/loki/clients/pkg/promtail/targets/syslog/syslogparser"
	"github.com/grafana/loki/clients/pkg/promtail/targets/target"

	"github.com/grafana/loki/pkg/logproto"
)

var (
	defaultIdleTimeout      = 120 * time.Second
	defaultMaxMessageLength = 8192
)

// SyslogTarget listens to syslog messages.
// nolint:revive
type SyslogTarget struct {
	metrics       *Metrics
	logger        log.Logger
	handler       api.EntryHandler
	config        *scrapeconfig.SyslogTargetConfig
	relabelConfig []*relabel.Config

	listener net.Listener
	messages chan message

	ctx             context.Context
	ctxCancel       context.CancelFunc
	openConnections *sync.WaitGroup
}

type message struct {
	labels    model.LabelSet
	message   string
	timestamp time.Time
}

// NewSyslogTarget configures a new SyslogTarget.
func NewSyslogTarget(
	metrics *Metrics,
	logger log.Logger,
	handler api.EntryHandler,
	relabel []*relabel.Config,
	config *scrapeconfig.SyslogTargetConfig,
) (*SyslogTarget, error) {

	ctx, cancel := context.WithCancel(context.Background())

	t := &SyslogTarget{
		metrics:       metrics,
		logger:        logger,
		handler:       handler,
		config:        config,
		relabelConfig: relabel,

		ctx:             ctx,
		ctxCancel:       cancel,
		openConnections: new(sync.WaitGroup),
	}

	t.messages = make(chan message)
	go t.messageSender(handler.Chan())

	err := t.run()
	return t, err
}

func (t *SyslogTarget) run() error {
	l, err := net.Listen("tcp", t.config.ListenAddress)
	l = conntrack.NewListener(l, conntrack.TrackWithName("syslog_target/"+t.config.ListenAddress))
	if err != nil {
		return fmt.Errorf("error setting up syslog target: %w", err)
	}

	tlsEnabled := t.config.TLSConfig.CertFile != "" || t.config.TLSConfig.KeyFile != "" || t.config.TLSConfig.CAFile != ""
	if tlsEnabled {
		tlsConfig, err := newTLSConfig(t.config.TLSConfig.CertFile, t.config.TLSConfig.KeyFile, t.config.TLSConfig.CAFile)
		if err != nil {
			return fmt.Errorf("error setting up syslog target: %w", err)
		}
		l = tls.NewListener(l, tlsConfig)
	}

	t.listener = l
	level.Info(t.logger).Log("msg", "syslog listening on address", "address", t.ListenAddress().String(), "tls", tlsEnabled)

	t.openConnections.Add(1)
	go t.acceptConnections()

	return nil
}

func newTLSConfig(certFile string, keyFile string, caFile string) (*tls.Config, error) {
	if certFile == "" || keyFile == "" {
		return nil, fmt.Errorf("certificate and key files are required")
	}

	certs, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("unable to load server certificate or key: %w", err)
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{certs},
	}

	if caFile != "" {
		caCert, err := ioutil.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("unable to load client CA certificate: %w", err)
		}

		caCertPool := x509.NewCertPool()
		if ok := caCertPool.AppendCertsFromPEM(caCert); !ok {
			return nil, fmt.Errorf("unable to parse client CA certificate")
		}

		tlsConfig.ClientCAs = caCertPool
		tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
	}

	return tlsConfig, nil
}

func (t *SyslogTarget) acceptConnections() {
	defer t.openConnections.Done()

	l := log.With(t.logger, "address", t.listener.Addr().String())

	backoff := backoff.New(t.ctx, backoff.Config{
		MinBackoff: 5 * time.Millisecond,
		MaxBackoff: 1 * time.Second,
	})

	for {
		c, err := t.listener.Accept()
		if err != nil {
			if t.ctx.Err() != nil {
				level.Info(l).Log("msg", "syslog server shutting down")
				return
			}

			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				level.Warn(l).Log("msg", "failed to accept syslog connection", "err", err, "num_retries", backoff.NumRetries())
				backoff.Wait()
				continue
			}

			level.Error(l).Log("msg", "failed to accept syslog connection. quiting", "err", err)
			return
		}
		backoff.Reset()

		t.openConnections.Add(1)
		go t.handleConnection(c)
	}

}

func (t *SyslogTarget) handleConnection(cn net.Conn) {
	defer t.openConnections.Done()

	c := &idleTimeoutConn{cn, t.idleTimeout()}

	handlerCtx, cancel := context.WithCancel(t.ctx)
	defer cancel()
	go func() {
		<-handlerCtx.Done()
		_ = c.Close()
	}()

	connLabels := t.connectionLabels(c)

	err := syslogparser.ParseStream(c, func(result *syslog.Result) {
		if err := result.Error; err != nil {
			t.handleMessageError(err)
			return
		}
		t.handleMessage(connLabels.Copy(), result.Message)
	}, t.maxMessageLength())

	if err != nil {
		level.Warn(t.logger).Log("msg", "error initializing syslog stream", "err", err)
	}
}

func (t *SyslogTarget) handleMessageError(err error) {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		level.Debug(t.logger).Log("msg", "connection timed out", "err", ne)
		return
	}
	level.Warn(t.logger).Log("msg", "error parsing syslog stream", "err", err)
	t.metrics.syslogParsingErrors.Inc()
}

func (t *SyslogTarget) handleMessage(connLabels labels.Labels, msg syslog.Message) {
	rfc5424Msg := msg.(*rfc5424.SyslogMessage)

	if rfc5424Msg.Message == nil {
		t.metrics.syslogEmptyMessages.Inc()
		return
	}

	lb := labels.NewBuilder(connLabels)
	if v := rfc5424Msg.SeverityLevel(); v != nil {
		lb.Set("__syslog_message_severity", *v)
	}
	if v := rfc5424Msg.FacilityLevel(); v != nil {
		lb.Set("__syslog_message_facility", *v)
	}
	if v := rfc5424Msg.Hostname; v != nil {
		lb.Set("__syslog_message_hostname", *v)
	}
	if v := rfc5424Msg.Appname; v != nil {
		lb.Set("__syslog_message_app_name", *v)
	}
	if v := rfc5424Msg.ProcID; v != nil {
		lb.Set("__syslog_message_proc_id", *v)
	}
	if v := rfc5424Msg.MsgID; v != nil {
		lb.Set("__syslog_message_msg_id", *v)
	}

	if t.config.LabelStructuredData && rfc5424Msg.StructuredData != nil {
		for id, params := range *rfc5424Msg.StructuredData {
			id = strings.Replace(id, "@", "_", -1)
			for name, value := range params {
				key := "__syslog_message_sd_" + id + "_" + name
				lb.Set(key, value)
			}
		}
	}

	processed := relabel.Process(lb.Labels(), t.relabelConfig...)

	filtered := make(model.LabelSet)
	for _, lbl := range processed {
		if strings.HasPrefix(lbl.Name, "__") {
			continue
		}
		filtered[model.LabelName(lbl.Name)] = model.LabelValue(lbl.Value)
	}

	var timestamp time.Time
	if t.config.UseIncomingTimestamp && rfc5424Msg.Timestamp != nil {
		timestamp = *rfc5424Msg.Timestamp
	} else {
		timestamp = time.Now()
	}

	m := *rfc5424Msg.Message
	if t.config.UseRFC5424Message {
		fullMsg, err := rfc5424Msg.String()
		if err != nil {
			level.Debug(t.logger).Log("msg", "failed to convert rfc5424 message to string; using message field instead", "err", err)
		} else {
			m = fullMsg
		}
	}
	t.messages <- message{filtered, m, timestamp}
}

func (t *SyslogTarget) messageSender(entries chan<- api.Entry) {
	for msg := range t.messages {
		entries <- api.Entry{
			Labels: msg.labels,
			Entry: logproto.Entry{
				Timestamp: msg.timestamp,
				Line:      msg.message,
			},
		}
		t.metrics.syslogEntries.Inc()
	}
}

func (t *SyslogTarget) connectionLabels(c net.Conn) labels.Labels {
	lb := labels.NewBuilder(nil)
	for k, v := range t.config.Labels {
		lb.Set(string(k), string(v))
	}

	ip := ipFromConn(c).String()
	lb.Set("__syslog_connection_ip_address", ip)
	lb.Set("__syslog_connection_hostname", lookupAddr(ip))

	return lb.Labels()
}

func ipFromConn(c net.Conn) net.IP {
	switch addr := c.RemoteAddr().(type) {
	case *net.TCPAddr:
		return addr.IP
	}

	return nil
}

func lookupAddr(addr string) string {
	names, _ := net.LookupAddr(addr)
	return strings.Join(names, ",")
}

// Type returns SyslogTargetType.
func (t *SyslogTarget) Type() target.TargetType {
	return target.SyslogTargetType
}

// Ready indicates whether or not the syslog target is ready to be read from.
func (t *SyslogTarget) Ready() bool {
	return true
}

// DiscoveredLabels returns the set of labels discovered by the syslog target, which
// is always nil. Implements Target.
func (t *SyslogTarget) DiscoveredLabels() model.LabelSet {
	return nil
}

// Labels returns the set of labels that statically apply to all log entries
// produced by the SyslogTarget.
func (t *SyslogTarget) Labels() model.LabelSet {
	return t.config.Labels
}

// Details returns target-specific details.
func (t *SyslogTarget) Details() interface{} {
	return map[string]string{}
}

// Stop shuts down the SyslogTarget.
func (t *SyslogTarget) Stop() error {
	t.ctxCancel()
	err := t.listener.Close()
	t.openConnections.Wait()
	close(t.messages)
	t.handler.Stop()
	return err
}

// ListenAddress returns the address SyslogTarget is listening on.
func (t *SyslogTarget) ListenAddress() net.Addr {
	return t.listener.Addr()
}

func (t *SyslogTarget) idleTimeout() time.Duration {
	if t.config.IdleTimeout != 0 {
		return t.config.IdleTimeout
	}
	return defaultIdleTimeout
}

func (t *SyslogTarget) maxMessageLength() int {
	if t.config.MaxMessageLength != 0 {
		return t.config.MaxMessageLength
	}
	return defaultMaxMessageLength
}

type idleTimeoutConn struct {
	net.Conn
	idleTimeout time.Duration
}

func (c *idleTimeoutConn) Write(p []byte) (int, error) {
	c.setDeadline()
	return c.Conn.Write(p)
}

func (c *idleTimeoutConn) Read(b []byte) (int, error) {
	c.setDeadline()
	return c.Conn.Read(b)
}

func (c *idleTimeoutConn) setDeadline() {
	_ = c.Conn.SetDeadline(time.Now().Add(c.idleTimeout))
}
