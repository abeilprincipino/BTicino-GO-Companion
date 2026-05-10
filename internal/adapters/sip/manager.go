package sip

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
	gosip "github.com/emiago/sipgo/sip"

	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/domain/event"
	psip "bticino-go-companion/internal/protocol/sip"
)

var (
	ErrNoIncomingCall = errors.New("no incoming call")
	ErrNoActiveCall   = errors.New("no active call")
)

const sipAnswerTimeout = 8 * time.Second

type Manager struct {
	cfg    config.Config
	logger *log.Logger

	enabled bool

	ua      *sipgo.UserAgent
	srv     *sipgo.Server
	client  *sipgo.Client
	dialogs *sipgo.DialogServerCache
	out     *sipgo.DialogClientCache

	mu        sync.Mutex
	sink      func(event.Envelope)
	incoming  *sipgo.DialogServerSession
	activeIn  *sipgo.DialogServerSession
	activeOut *sipgo.DialogClientSession
	dialing   bool
}

func NewManager(cfg config.Config, logger *log.Logger) *Manager {
	return &Manager{
		cfg:     cfg,
		logger:  logger,
		enabled: cfg.MediaSIPEnabled,
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

	fromUser, fromHost, _ := psip.ParseFromAddress(m.cfg.MediaSIPFrom)
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

	m.logf("sip manager started transport=%s listen=%s from=%s", transport, listenAddr, m.cfg.MediaSIPFrom)
	return nil
}

func (m *Manager) registerHandlers() {
	m.srv.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
		dlg, err := m.dialogs.ReadInvite(req, tx)
		if err != nil {
			_ = tx.Respond(gosip.NewResponseFromRequest(req, 500, "Server Error", nil))
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
		m.mu.Unlock()
		m.publish("call.incoming", map[string]any{"source": "sip", "raw": req.StartLine()})
	})

	m.srv.OnCancel(func(req *gosip.Request, tx gosip.ServerTransaction) {
		_ = tx.Respond(gosip.NewResponseFromRequest(req, 200, "OK", nil))
		m.mu.Lock()
		hadIncoming := m.incoming != nil
		if m.incoming != nil {
			_ = m.incoming.Close()
			m.incoming = nil
		}
		m.mu.Unlock()
		if hadIncoming {
			m.publish("call.ended", map[string]any{"source": "sip", "reason": "cancel"})
		}
	})

	m.srv.OnAck(func(req *gosip.Request, tx gosip.ServerTransaction) {
		_ = m.dialogs.ReadAck(req, tx)
	})

	m.srv.OnBye(func(req *gosip.Request, tx gosip.ServerTransaction) {
		if m.out != nil {
			if err := m.out.ReadBye(req, tx); err == nil {
				m.mu.Lock()
				if m.activeOut != nil {
					_ = m.activeOut.Close()
				}
				m.activeOut = nil
				m.mu.Unlock()
				m.publish("call.ended", map[string]any{"source": "sip", "reason": "remote_bye_outgoing"})
				return
			}
		}

		if err := m.dialogs.ReadBye(req, tx); err != nil {
			_ = tx.Respond(gosip.NewResponseFromRequest(req, 200, "OK", nil))
		}
		m.mu.Lock()
		if m.activeIn != nil {
			_ = m.activeIn.Close()
		}
		m.activeIn = nil
		m.incoming = nil
		m.mu.Unlock()
		m.publish("call.ended", map[string]any{"source": "sip", "reason": "remote_bye"})
	})
}

func (m *Manager) Enabled() bool {
	return m.enabled
}

func (m *Manager) HasIncomingCall() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.incoming != nil
}

func (m *Manager) HasActiveCall() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeIn != nil || m.activeOut != nil || m.dialing
}

func (m *Manager) Answer(_ context.Context) error {
	if !m.enabled {
		return ErrNoIncomingCall
	}

	m.mu.Lock()
	dlg := m.incoming
	m.mu.Unlock()
	if dlg == nil {
		return ErrNoIncomingCall
	}

	if err := dlg.RespondSDP([]byte(m.answerSDP())); err != nil {
		return fmt.Errorf("answer failed: %w", err)
	}

	m.mu.Lock()
	m.activeIn = dlg
	m.incoming = nil
	m.mu.Unlock()
	m.publish("call.answered", map[string]any{"source": "sip", "mode": "incoming"})
	return nil
}

func (m *Manager) Hangup(ctx context.Context) error {
	if !m.enabled {
		return ErrNoActiveCall
	}

	m.mu.Lock()
	incoming := m.incoming
	activeIn := m.activeIn
	activeOut := m.activeOut
	m.mu.Unlock()

	if incoming != nil {
		if err := incoming.Respond(487, "Request Terminated", nil); err != nil {
			return fmt.Errorf("reject incoming failed: %w", err)
		}
		_ = incoming.Close()
		m.mu.Lock()
		if m.incoming == incoming {
			m.incoming = nil
		}
		m.mu.Unlock()
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
	return nil
}

func (m *Manager) StreamStart(ctx context.Context) error {
	if !m.enabled {
		return ErrNoActiveCall
	}
	if m.out == nil {
		return ErrNoActiveCall
	}

	target, err := psip.ResolveInviteTarget(m.cfg.MediaSIPTo, m.cfg.MediaSIPDomain, true)
	if err != nil {
		return err
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
		if err := incoming.RespondSDP([]byte(m.answerSDP())); err != nil {
			return fmt.Errorf("answer incoming for stream failed: %w", err)
		}
		m.mu.Lock()
		if m.incoming == incoming {
			m.incoming = nil
		}
		m.activeIn = incoming
		m.mu.Unlock()
		m.publish("call.answered", map[string]any{"source": "sip", "mode": "incoming"})
		return nil
	}

	inviteReq := gosip.NewRequest(gosip.INVITE, target.URI)
	inviteReq.SetTransport(strings.ToUpper(normalizeTransport(m.cfg.MediaSIPTransport)))
	if target.Destination != "" {
		inviteReq.SetDestination(target.Destination)
	}
	inviteReq.AppendHeader(gosip.NewHeader("Content-Type", "application/sdp"))
	inviteReq.SetBody([]byte(m.offerSDP(target.AddDevAddr)))

	callCtx, cancel := context.WithTimeout(ctx, sipAnswerTimeout)
	defer cancel()

	dlg, err := m.out.WriteInvite(callCtx, inviteReq)
	if err != nil {
		return fmt.Errorf("outgoing invite failed: %w", err)
	}

	opts := sipgo.AnswerOptions{
		Username: strings.TrimSpace(m.cfg.MediaSIPAuthUser),
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
	m.publish("call.answered", map[string]any{"source": "sip", "mode": "outgoing", "target": target.URI.String()})
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
	if m.srv != nil {
		_ = m.srv.Close()
	}
	if m.ua != nil {
		return m.ua.Close()
	}
	return nil
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
		TS:      time.Now().UTC(),
		Source:  event.SourceSIP,
		Payload: payload,
	})
}

func (m *Manager) offerSDP(includeDevAddr bool) string {
	host, _ := hostFromListen(m.cfg.MediaSIPListen)
	return psip.BuildOffer(psip.SDPConfig{
		Host:           host,
		AudioPort:      65000,
		VideoPort:      65002,
		IncludeDevAddr: includeDevAddr,
		DevAddr:        m.cfg.MediaSIPStreamDevAddr,
	})
}

func (m *Manager) answerSDP() string {
	host, _ := hostFromListen(m.cfg.MediaSIPListen)
	return psip.BuildAnswer(psip.SDPConfig{
		Host:      host,
		AudioPort: 65000,
		VideoPort: 65002,
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

func buildContactHeader(cfg config.Config) gosip.ContactHeader {
	user, host, port := psip.ParseFromAddress(cfg.MediaSIPFrom)
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
	return gosip.ContactHeader{Address: gosip.Uri{User: user, Host: host, Port: port}}
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
