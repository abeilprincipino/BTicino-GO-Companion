package signaling

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

const streamAnswerTimeout = 8 * time.Second

var (
	ErrStreamTargetUnset              = errors.New("sip: stream target not configured")
	_                    StreamDialer = (*streamDialer)(nil)
)

// StreamDialerConfig contains the outbound SIP settings required for a stream.
// Registration and inbound SIP handling are deliberately outside this dialer.
type StreamDialerConfig struct {
	Target            string
	Domain            string
	From              string
	AuthUser          string
	AuthPass          string
	Transport         string
	Listen            string
	Logger            *slog.Logger
	RemoteDialogEnded func()
}

type streamDialer struct {
	ua                *sipgo.UserAgent
	server            *sipgo.Server
	out               *sipgo.DialogClientCache
	target            inviteTarget
	authUser          string
	authPass          string
	transport         string
	logger            *slog.Logger
	remoteDialogEnded func()
	listenerCancel    context.CancelFunc
	closeOnce         sync.Once
	closeErr          error
}

func NewStreamDialer(cfg StreamDialerConfig) (*streamDialer, error) {
	target, err := resolveInviteTarget(cfg.Target, cfg.Domain)
	if err != nil {
		return nil, err
	}

	fromUser, fromHost, fromPort := parseAddress(firstNonEmpty(cfg.From, "companion@127.0.0.1"))
	if fromUser == "" {
		return nil, fmt.Errorf("sip: invalid from address")
	}
	if fromHost == "" {
		fromHost = "127.0.0.1"
	}
	if fromPort == 0 {
		_, fromPort = hostPort(cfg.Listen)
	}
	if fromPort == 0 {
		fromPort = 5070
	}

	ua, err := sipgo.NewUA(sipgo.WithUserAgent(fromUser), sipgo.WithUserAgentHostname(fromHost))
	if err != nil {
		return nil, fmt.Errorf("create sip user agent: %w", err)
	}
	client, err := sipgo.NewClient(ua)
	if err != nil {
		_ = ua.Close()
		return nil, fmt.Errorf("create sip client: %w", err)
	}
	server, err := sipgo.NewServer(ua)
	if err != nil {
		_ = ua.Close()
		return nil, fmt.Errorf("create sip server: %w", err)
	}

	contact := sip.ContactHeader{Address: sip.Uri{User: fromUser, Host: fromHost, Port: fromPort}}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	listenerCtx, listenerCancel := context.WithCancel(context.Background())
	dialer := &streamDialer{
		ua:                ua,
		server:            server,
		out:               sipgo.NewDialogClientCache(client, contact),
		target:            target,
		authUser:          firstNonEmpty(cfg.AuthUser, fromUser),
		authPass:          strings.TrimSpace(cfg.AuthPass),
		transport:         normalizeTransport(cfg.Transport),
		logger:            logger.With("component", "media.sip", "target", target.URI.User+"@"+target.URI.Host, "domain", strings.TrimSpace(cfg.Domain), "transport", normalizeTransport(cfg.Transport)),
		remoteDialogEnded: cfg.RemoteDialogEnded,
		listenerCancel:    listenerCancel,
	}
	server.OnBye(dialer.onBye)

	listenAddr := strings.TrimSpace(cfg.Listen)
	if listenAddr == "" {
		listenAddr = net.JoinHostPort(fromHost, strconv.Itoa(fromPort))
	}
	go dialer.listen(listenerCtx, listenAddr)

	return dialer, nil
}

func (d *streamDialer) Close() error {
	d.closeOnce.Do(func() {
		d.listenerCancel()
		d.closeErr = errors.Join(d.server.Close(), d.ua.Close())
	})
	return d.closeErr
}

func (d *streamDialer) listen(ctx context.Context, addr string) {
	if err := d.server.ListenAndServe(ctx, d.transport, addr); err != nil && ctx.Err() == nil {
		d.logger.Warn("sip listener stopped", "error", err)
	}
}

func (d *streamDialer) onBye(req *sip.Request, tx sip.ServerTransaction) {
	if err := d.out.ReadBye(req, tx); err != nil {
		d.logger.Warn("remote sip bye rejected", "error", err)
		return
	}

	d.logger.Info("remote sip stream ended")
	if d.remoteDialogEnded != nil {
		d.remoteDialogEnded()
	}
}

func (d *streamDialer) StartStream(ctx context.Context, _ string, offer string) (OutgoingDialog, error) {
	d.logger.InfoContext(ctx, "sip stream dial starting")
	req := sip.NewRequest(sip.INVITE, d.target.URI)
	req.SetTransport(strings.ToUpper(d.transport))
	if d.target.destination != "" {
		req.SetDestination(d.target.destination)
	}
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.SetBody([]byte(offer))

	callCtx, cancel := context.WithTimeout(ctx, streamAnswerTimeout)
	defer cancel()

	dialog, err := d.out.WriteInvite(callCtx, req)
	if err != nil {
		return nil, fmt.Errorf("outgoing invite: %w", err)
	}
	if err := dialog.WaitAnswer(callCtx, sipgo.AnswerOptions{Username: d.authUser, Password: d.authPass}); err != nil {
		_ = dialog.Close()
		return nil, fmt.Errorf("wait for invite answer: %w", err)
	}
	if err := dialog.Ack(callCtx); err != nil {
		_ = dialog.Close()
		return nil, fmt.Errorf("ack invite: %w", err)
	}

	d.logger.InfoContext(ctx, "sip stream established")
	return outgoingDialog{dialog: dialog, logger: d.logger}, nil
}

type outgoingDialog struct {
	dialog *sipgo.DialogClientSession
	logger *slog.Logger
}

func (d outgoingDialog) Bye(ctx context.Context) error {
	d.logger.InfoContext(ctx, "sip stream bye starting")
	err := d.dialog.Bye(ctx)
	_ = d.dialog.Close()
	if err == nil {
		d.logger.InfoContext(ctx, "sip stream bye completed")
	}
	return err
}

type inviteTarget struct {
	URI         sip.Uri
	destination string
}

func resolveInviteTarget(rawTarget, domain string) (inviteTarget, error) {
	rawTarget = strings.TrimSpace(rawTarget)
	if rawTarget == "" {
		return inviteTarget{}, ErrStreamTargetUnset
	}

	hadAt := strings.Contains(rawTarget, "@")
	if !strings.HasPrefix(strings.ToLower(rawTarget), "sip:") && !strings.HasPrefix(strings.ToLower(rawTarget), "sips:") {
		rawTarget = "sip:" + rawTarget
	}

	var uri sip.Uri
	if err := sip.ParseUri(rawTarget, &uri); err != nil {
		return inviteTarget{}, fmt.Errorf("parse stream target: %w", err)
	}

	destination := uriHostPort(uri)
	domain = strings.TrimSpace(domain)
	if !hadAt && uri.User == "" && uri.Host != "" && domain != "" {
		uri.User, uri.Host = uri.Host, domain
	}
	if domain != "" && isIPAddressOrLocal(uri.Host) {
		uri.Host = domain
	}
	if uri.Host == "" || uri.User == "" {
		return inviteTarget{}, ErrStreamTargetUnset
	}

	return inviteTarget{URI: uri, destination: destination}, nil
}

func parseAddress(raw string) (string, string, int) {
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "sip:")
	parts := strings.SplitN(raw, "@", 2)
	if len(parts) != 2 {
		return "", "", 0
	}
	user, host := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	parsedHost, port, err := net.SplitHostPort(host)
	if err == nil {
		return user, parsedHost, portNumber(port)
	}
	return user, host, 0
}

func hostPort(raw string) (string, int) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(raw))
	if err != nil {
		return "", 0
	}
	return host, portNumber(port)
}

func portNumber(raw string) int {
	port, _ := strconv.Atoi(raw)
	return port
}

func uriHostPort(uri sip.Uri) string {
	if uri.Host == "" {
		return ""
	}
	if uri.Port == 0 {
		return net.JoinHostPort(uri.Host, "5060")
	}
	return net.JoinHostPort(uri.Host, strconv.Itoa(uri.Port))
}

func isIPAddressOrLocal(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	return host == "localhost" || host == "0.0.0.0" || net.ParseIP(host) != nil
}

func normalizeTransport(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "udp", "tcp", "ws", "wss", "tls":
		return strings.ToLower(strings.TrimSpace(raw))
	default:
		return "tcp"
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
