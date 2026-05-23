package sipadapter

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/domain/event"
	"bticino-go-companion/internal/protocol/sip"
)

var (
	ErrNoIncomingCall = errors.New("no incoming call")
	ErrNoActiveCall   = errors.New("no active call")
)

const (
	registerCheckInterval = 10 * time.Second
	registerTimeout       = 4 * time.Second
	registerExpires       = 600
	registerSkew          = 10
	incomingInviteTimeout = 60 * time.Second
	sipAnswerTimeout      = 8 * time.Second
	sipSDPAudioPort       = 65000
	sipSDPVideoPort       = 65002
)

type Manager struct {
	cfg    config.Config
	logger *log.Logger

	enabled bool

	ua      *sipgo.UserAgent
	srv     *sipgo.Server
	client  *sipgo.Client
	dialogs *sipgo.DialogServerCache
	out     *sipgo.DialogClientCache

	mu               sync.Mutex
	sink             func(event.Envelope)
	incoming         *sipgo.DialogServerSession
	incomingExpiry   *time.Timer
	incomingExpiryID uint64
	incomingTimeout  time.Duration
	activeIn         *sipgo.DialogServerSession
	activeOut        *sipgo.DialogClientSession
	dialing          bool

	registerCancel context.CancelFunc
	registerWG     sync.WaitGroup
	registerEvery  time.Duration
	lastRegister   time.Time
}

func NewManager(cfg config.Config, logger *log.Logger) *Manager {
	return &Manager{
		cfg:             cfg,
		logger:          logger,
		enabled:         cfg.MediaSIPEnabled,
		incomingTimeout: incomingInviteTimeout,
	}
}

func (m *Manager) SetEventSink(sink func(event.Envelope)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sink = sink
}

func (m *Manager) Start(ctx context.Context) error {
	if !m.enabled {
		return nil
	}

	resolvedCfg, err := resolveSIPConfig(m.cfg)
	if err != nil {
		return fmt.Errorf("resolve sip profile: %w", err)
	}
	m.cfg = resolvedCfg

	fromUser, fromHost, _ := sipprotocol.ParseFromAddress(m.cfg.MediaSIPFrom)
	uaOpts := make([]sipgo.UserAgentOption, 0, 2)
	if fromUser != "" {
		uaOpts = append(uaOpts, sipgo.WithUserAgent(fromUser))
	}
	if fromHost != "" {
		uaOpts = append(uaOpts, sipgo.WithUserAgentHostname(fromHost))
	}

	ua, err := sipgo.NewUA(uaOpts...)
	if err != nil {
		return fmt.Errorf("create ua: %w", err)
	}
	m.ua = ua

	client, err := sipgo.NewClient(ua)
	if err != nil {
		_ = ua.Close()
		m.ua = nil
		return fmt.Errorf("create client: %w", err)
	}
	m.client = client

	srv, err := sipgo.NewServer(ua)
	if err != nil {
		_ = ua.Close()
		m.ua = nil
		m.client = nil
		return fmt.Errorf("create server: %w", err)
	}
	m.srv = srv

	contact := buildContactHeader(m.cfg)
	m.dialogs = sipgo.NewDialogServerCache(client, contact)
	m.out = sipgo.NewDialogClientCache(client, contact)

	m.registerHandlers()

	transport := normalizeTransport(m.cfg.MediaSIPTransport)
	listenAddr := strings.TrimSpace(m.cfg.MediaSIPListen)
	if listenAddr == "" {
		listenAddr = "0.0.0.0:5070"
	}

	go func() {
		if err := srv.ListenAndServe(ctx, transport, listenAddr); err != nil {
			if !strings.Contains(strings.ToLower(err.Error()), "closed") {
				m.logf("sip listen error: %v", err)
			}
		}
	}()

	m.registerEvery = time.Duration(registerExpires-registerSkew) * time.Second
	if m.registerEvery <= 0 {
		m.registerEvery = 10 * time.Minute
	}
	registerCtx, registerCancel := context.WithCancel(context.Background())
	m.registerCancel = registerCancel
	m.registerWG.Add(1)
	go m.registrationLoop(registerCtx)

	m.logf("sip manager started transport=%s listen=%s from=%s", transport, listenAddr, m.cfg.MediaSIPFrom)
	return nil
}

func (m *Manager) registerHandlers() {
	m.srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		dlg, err := m.dialogs.ReadInvite(req, tx)
		if err != nil {
			_ = tx.Respond(sip.NewResponseFromRequest(req, 500, "Server Error", nil))
			return
		}

		m.mu.Lock()
		busy := m.incoming != nil || m.activeIn != nil || m.activeOut != nil || m.dialing
		m.mu.Unlock()
		if busy {
			_ = dlg.Respond(486, "Busy Here", nil)
			_ = dlg.Close()
			return
		}

		if err := dlg.Respond(180, "Ringing", nil); err != nil {
			m.logf("sip ringing failed: %v", err)
		}

		m.mu.Lock()
		m.incoming = dlg
		m.startIncomingExpiryLocked(dlg)
		m.mu.Unlock()
		m.publish(event.TypeCallIncoming, map[string]any{"source": "sip", "raw": req.StartLine()})
	})

	m.srv.OnCancel(func(req *sip.Request, tx sip.ServerTransaction) {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
		m.mu.Lock()
		hadIncoming := m.incoming != nil
		if m.incoming != nil {
			_ = m.incoming.Close()
			m.incoming = nil
			m.stopIncomingExpiryLocked()
		}
		m.mu.Unlock()
		if hadIncoming {
			m.publish(event.TypeCallEnded, map[string]any{"source": "sip", "reason": "cancel"})
		}
	})

	m.srv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {
		_ = m.dialogs.ReadAck(req, tx)
	})

	m.srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		if m.out != nil {
			if err := m.out.ReadBye(req, tx); err == nil {
				m.mu.Lock()
				if m.activeOut != nil {
					_ = m.activeOut.Close()
				}
				m.activeOut = nil
				m.mu.Unlock()
				m.publish(event.TypeStreamStopped, map[string]any{"source": "sip", "reason": "remote_bye_outgoing"})
				m.publish(event.TypeCallEnded, map[string]any{"source": "sip", "reason": "remote_bye_outgoing"})
				return
			}
		}

		if err := m.dialogs.ReadBye(req, tx); err != nil {
			_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
		}
		m.mu.Lock()
		if m.activeIn != nil {
			_ = m.activeIn.Close()
		}
		m.activeIn = nil
		m.incoming = nil
		m.stopIncomingExpiryLocked()
		m.mu.Unlock()
		m.publish(event.TypeStreamStopped, map[string]any{"source": "sip", "reason": "remote_bye"})
		m.publish(event.TypeCallEnded, map[string]any{"source": "sip", "reason": "remote_bye"})
	})
}

func (m *Manager) Hangup(ctx context.Context) error {
	if !m.enabled {
		return ErrNoActiveCall
	}

	m.mu.Lock()
	incoming := m.incoming
	activeIn := m.activeIn
	activeOut := m.activeOut
	if incoming != nil {
		m.stopIncomingExpiryLocked()
	}
	m.mu.Unlock()

	if incoming != nil {
		if err := incoming.Respond(487, "Request Terminated", nil); err != nil {
			m.mu.Lock()
			if m.incoming == incoming {
				m.startIncomingExpiryLocked(incoming)
			}
			m.mu.Unlock()
			return fmt.Errorf("reject incoming failed: %w", err)
		}
		_ = incoming.Close()
		m.mu.Lock()
		if m.incoming == incoming {
			m.incoming = nil
		}
		m.mu.Unlock()
		m.publish(event.TypeCallEnded, map[string]any{"source": "sip", "reason": "local_reject_incoming"})
		return nil
	}

	if activeIn == nil && activeOut == nil {
		return ErrNoActiveCall
	}

	if activeOut != nil {
		if err := activeOut.Bye(ctx); err != nil {
			return fmt.Errorf("outgoing bye failed: %w", err)
		}
		_ = activeOut.Close()
		m.mu.Lock()
		if m.activeOut == activeOut {
			m.activeOut = nil
		}
		m.mu.Unlock()
		m.publish(event.TypeStreamStopped, map[string]any{"source": "sip", "reason": "local_hangup_outgoing"})
		m.publish(event.TypeCallEnded, map[string]any{"source": "sip", "reason": "local_hangup_outgoing"})
		return nil
	}

	if err := activeIn.Bye(ctx); err != nil {
		return fmt.Errorf("incoming bye failed: %w", err)
	}
	_ = activeIn.Close()
	m.mu.Lock()
	if m.activeIn == activeIn {
		m.activeIn = nil
	}
	m.mu.Unlock()
	m.publish(event.TypeStreamStopped, map[string]any{"source": "sip", "reason": "local_hangup_incoming"})
	m.publish(event.TypeCallEnded, map[string]any{"source": "sip", "reason": "local_hangup_incoming"})
	return nil
}

func (m *Manager) Answer(ctx context.Context) error {
	if !m.enabled {
		return ErrNoIncomingCall
	}
	_ = ctx

	m.mu.Lock()
	incoming := m.incoming
	if incoming != nil {
		m.stopIncomingExpiryLocked()
	}
	m.mu.Unlock()
	if incoming == nil {
		return ErrNoIncomingCall
	}

	if err := incoming.RespondSDP([]byte(m.answerSDP())); err != nil {
		m.mu.Lock()
		if m.incoming == incoming {
			m.startIncomingExpiryLocked(incoming)
		}
		m.mu.Unlock()
		return fmt.Errorf("answer incoming failed: %w", err)
	}
	m.mu.Lock()
	answered := false
	if m.incoming == incoming {
		m.incoming = nil
		m.activeIn = incoming
		answered = true
	}
	m.mu.Unlock()
	if !answered {
		_ = incoming.Close()
		return ErrNoIncomingCall
	}
	m.publish(event.TypeCallAnswered, map[string]any{"source": "sip", "mode": "incoming"})
	return nil
}

func (m *Manager) StreamStart(ctx context.Context, devAddr string) error {
	if !m.enabled {
		return ErrNoActiveCall
	}
	if m.out == nil {
		return ErrNoActiveCall
	}

	target, err := sipprotocol.ResolveInviteTarget(m.cfg.MediaSIPTo, m.cfg.MediaSIPDomain, true)
	if err != nil {
		return err
	}
	if target.AddDevAddr && strings.TrimSpace(devAddr) == "" {
		return errors.New("empty stream devaddr")
	}

	m.mu.Lock()
	incoming := m.incoming
	if m.activeIn != nil || m.activeOut != nil || m.dialing {
		m.mu.Unlock()
		return nil
	}
	m.dialing = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.dialing = false
		m.mu.Unlock()
	}()

	if incoming != nil {
		return m.Answer(ctx)
	}

	inviteReq := sip.NewRequest(sip.INVITE, target.URI)
	inviteReq.SetTransport(strings.ToUpper(normalizeTransport(m.cfg.MediaSIPTransport)))
	if target.Destination != "" {
		inviteReq.SetDestination(target.Destination)
	}
	inviteReq.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	inviteReq.SetBody([]byte(m.offerSDP(target.AddDevAddr, devAddr)))

	callCtx, cancel := context.WithTimeout(ctx, sipAnswerTimeout)
	defer cancel()

	dlg, err := m.out.WriteInvite(callCtx, inviteReq)
	if err != nil {
		return fmt.Errorf("outgoing invite failed: %w", err)
	}

	opts := sipgo.AnswerOptions{
		Username: inviteAuthUser(m.cfg),
		Password: strings.TrimSpace(m.cfg.MediaSIPAuthPass),
	}
	if err := dlg.WaitAnswer(callCtx, opts); err != nil {
		_ = dlg.Close()
		return fmt.Errorf("wait answer failed: %w", err)
	}
	if err := dlg.Ack(callCtx); err != nil {
		_ = dlg.Close()
		return fmt.Errorf("ack failed: %w", err)
	}

	m.mu.Lock()
	m.activeOut = dlg
	m.mu.Unlock()
	m.publish(event.TypeCallAnswered, map[string]any{"source": "sip", "mode": "outgoing", "target": target.URI.String()})
	return nil
}

func (m *Manager) StreamStop(ctx context.Context) error {
	err := m.Hangup(ctx)
	if errors.Is(err, ErrNoActiveCall) {
		return nil
	}
	return err
}

func (m *Manager) Close() error {
	m.mu.Lock()
	m.stopIncomingExpiryLocked()
	m.mu.Unlock()

	if m.registerCancel != nil {
		m.registerCancel()
	}
	m.registerWG.Wait()

	if m.srv != nil {
		_ = m.srv.Close()
	}
	if m.ua != nil {
		return m.ua.Close()
	}
	return nil
}

func (m *Manager) startIncomingExpiryLocked(dlg *sipgo.DialogServerSession) {
	m.stopIncomingExpiryLocked()

	timeout := m.incomingTimeout
	if timeout <= 0 {
		timeout = incomingInviteTimeout
	}

	m.incomingExpiryID++
	expiryID := m.incomingExpiryID
	m.incomingExpiry = time.AfterFunc(timeout, func() {
		expired := false

		m.mu.Lock()
		if m.incomingExpiryID == expiryID && m.incoming == dlg {
			if m.incoming != nil {
				_ = m.incoming.Close()
			}
			m.incoming = nil
			m.incomingExpiry = nil
			expired = true
		}
		m.mu.Unlock()

		if expired {
			m.publish(event.TypeCallEnded, map[string]any{"source": "sip", "reason": "incoming_timeout"})
		}
	})
}

func (m *Manager) stopIncomingExpiryLocked() {
	if m.incomingExpiry != nil {
		m.incomingExpiry.Stop()
		m.incomingExpiry = nil
	}
	m.incomingExpiryID++
}

func (m *Manager) publish(kind string, payload map[string]any) {
	m.mu.Lock()
	sink := m.sink
	m.mu.Unlock()
	if sink == nil {
		return
	}
	sink(event.Envelope{
		Type:    kind,
		TS:      time.Now(),
		Source:  event.SourceSIP,
		Payload: payload,
	})
}

func (m *Manager) offerSDP(includeDevAddr bool, devAddr string) string {
	host, _ := hostFromListen(m.cfg.MediaSIPListen)
	return sipprotocol.BuildOffer(sipprotocol.SDPConfig{
		Host:           host,
		AudioPort:      sipSDPAudioPort,
		VideoPort:      sipSDPVideoPort,
		IncludeDevAddr: includeDevAddr,
		DevAddr:        strings.TrimSpace(devAddr),
	})
}

func (m *Manager) answerSDP() string {
	host, _ := hostFromListen(m.cfg.MediaSIPListen)
	return sipprotocol.BuildAnswer(sipprotocol.SDPConfig{
		Host:      host,
		AudioPort: sipSDPAudioPort,
		VideoPort: sipSDPVideoPort,
	})
}

func normalizeTransport(raw string) string {
	val := strings.ToLower(strings.TrimSpace(raw))
	switch val {
	case "udp", "tcp", "ws", "wss", "tls":
		return val
	default:
		return "tcp"
	}
}

func buildContactHeader(cfg config.Config) sip.ContactHeader {
	user, host, port := sipprotocol.ParseFromAddress(cfg.MediaSIPFrom)
	if host == "" {
		host, _ = hostFromListen(cfg.MediaSIPListen)
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	if port <= 0 {
		_, p := hostFromListen(cfg.MediaSIPListen)
		port = p
	}
	if port <= 0 {
		port = 5070
	}
	if user == "" {
		user = "webrtc"
	}
	return sip.ContactHeader{Address: sip.Uri{User: user, Host: host, Port: port}}
}

func inviteAuthUser(cfg config.Config) string {
	if user := strings.TrimSpace(cfg.MediaSIPAuthUser); user != "" {
		return user
	}
	fromUser, _, _ := sipprotocol.ParseFromAddress(cfg.MediaSIPFrom)
	return fromUser
}

func hostFromListen(raw string) (string, int) {
	addr := strings.TrimSpace(raw)
	if addr == "" {
		return "", 0
	}
	if strings.HasPrefix(addr, ":") {
		p, _ := strconv.Atoi(strings.TrimPrefix(addr, ":"))
		return "", p
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0
	}
	port, _ := strconv.Atoi(strings.TrimSpace(portStr))
	return strings.TrimSpace(host), port
}

func (m *Manager) logf(format string, args ...any) {
	if m.logger != nil {
		m.logger.Printf(format, args...)
	}
}

func (m *Manager) registrationLoop(ctx context.Context) {
	defer m.registerWG.Done()

	// Perform one immediate REGISTER attempt at startup.
	m.tryRegister(ctx, true)

	ticker := time.NewTicker(registerCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.tryRegister(ctx, false)
		}
	}
}

func (m *Manager) tryRegister(loopCtx context.Context, force bool) {
	if !m.shouldRegister(force) {
		return
	}

	ctx, cancel := context.WithTimeout(loopCtx, registerTimeout)
	defer cancel()

	if err := m.registerOnce(ctx); err != nil {
		m.logf("sip register failed: %v", err)
		return
	}

	m.mu.Lock()
	m.lastRegister = time.Now()
	m.mu.Unlock()
}

func (m *Manager) shouldRegister(force bool) bool {
	if m.client == nil {
		return false
	}
	if registerDomain(m.cfg) == "" {
		return false
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	busy := m.incoming != nil || m.activeIn != nil || m.activeOut != nil || m.dialing
	if busy {
		return false
	}

	if force || m.lastRegister.IsZero() {
		return true
	}
	return time.Since(m.lastRegister) >= m.registerEvery
}

func (m *Manager) registerOnce(ctx context.Context) error {
	domain := registerDomain(m.cfg)
	if domain == "" {
		return nil
	}

	fromUser, _, _ := sipprotocol.ParseFromAddress(m.cfg.MediaSIPFrom)
	if fromUser == "" {
		return fmt.Errorf("register from user missing")
	}

	req := sip.NewRequest(sip.REGISTER, sip.Uri{
		Scheme: "sip",
		Host:   domain,
	})
	req.SetTransport(strings.ToUpper(normalizeTransport(m.cfg.MediaSIPTransport)))
	req.AppendHeader(sip.NewHeader("To", fmt.Sprintf("<sip:%s@%s>", fromUser, domain)))
	req.AppendHeader(sip.NewHeader("From", fmt.Sprintf("<sip:%s@%s>;tag=%s", fromUser, domain, sip.GenerateTagN(16))))
	req.AppendHeader(sip.NewHeader("Contact", formatContactValue(buildContactHeader(m.cfg))))
	req.AppendHeader(sip.NewHeader("Expires", strconv.Itoa(registerExpires)))

	res, err := m.client.Do(ctx, req, sipgo.ClientRequestRegisterBuild)
	if err != nil {
		return err
	}
	if res == nil {
		return fmt.Errorf("empty register response")
	}

	if (res.StatusCode == sip.StatusUnauthorized || res.StatusCode == sip.StatusProxyAuthRequired) && strings.TrimSpace(m.cfg.MediaSIPAuthPass) != "" {
		authRes, authErr := m.client.DoDigestAuth(ctx, req, res, sipgo.DigestAuth{
			Username: inviteAuthUser(m.cfg),
			Password: strings.TrimSpace(m.cfg.MediaSIPAuthPass),
		})
		if authErr != nil {
			return fmt.Errorf("register digest auth failed: %w", authErr)
		}
		res = authRes
	}

	if !res.IsSuccess() {
		return fmt.Errorf("register failed status=%d", res.StatusCode)
	}
	return nil
}

func registerDomain(cfg config.Config) string {
	if domain := strings.TrimSpace(cfg.MediaSIPDomain); domain != "" {
		return domain
	}
	target, err := sipprotocol.ResolveInviteTarget(cfg.MediaSIPTo, cfg.MediaSIPDomain, false)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(target.URI.Host)
}

func formatContactValue(contact sip.ContactHeader) string {
	host := strings.TrimSpace(contact.Address.Host)
	if host == "" {
		host = "127.0.0.1"
	}
	port := contact.Address.Port
	if port <= 0 {
		port = 5070
	}
	user := strings.TrimSpace(contact.Address.User)
	if user == "" {
		user = "webrtc"
	}
	return fmt.Sprintf("<sip:%s@%s:%d>", user, host, port)
}
