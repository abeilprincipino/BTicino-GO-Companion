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

const (
	streamAnswerTimeout     = 8 * time.Second
	registerCheckInterval   = 10 * time.Second
	registerTimeout         = 4 * time.Second
	registerExpires         = 600 * time.Second
	registerRefreshSkew     = 10 * time.Second
	registerRefreshInterval = registerExpires - registerRefreshSkew
)

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
	client            *sipgo.Client
	out               *sipgo.DialogClientCache
	contact           sip.ContactHeader
	target            inviteTarget
	authUser          string
	authPass          string
	transport         string
	logger            *slog.Logger
	remoteDialogEnded func()
	callbackMu        sync.RWMutex
	listenerCancel    context.CancelFunc
	registerCancel    context.CancelFunc
	registerWG        sync.WaitGroup
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
	registerCtx, registerCancel := context.WithCancel(context.Background())
	dialer := &streamDialer{
		ua:                ua,
		server:            server,
		client:            client,
		out:               sipgo.NewDialogClientCache(client, contact),
		contact:           contact,
		target:            target,
		authUser:          firstNonEmpty(cfg.AuthUser, fromUser),
		authPass:          strings.TrimSpace(cfg.AuthPass),
		transport:         normalizeTransport(cfg.Transport),
		logger:            logger.With("component", "media.sip", "target", target.URI.User+"@"+target.URI.Host, "domain", strings.TrimSpace(cfg.Domain), "transport", normalizeTransport(cfg.Transport)),
		remoteDialogEnded: cfg.RemoteDialogEnded,
		listenerCancel:    listenerCancel,
		registerCancel:    registerCancel,
	}
	server.OnBye(dialer.onBye)

	listenAddr := strings.TrimSpace(cfg.Listen)
	if listenAddr == "" {
		listenAddr = net.JoinHostPort(fromHost, strconv.Itoa(fromPort))
	}
	go dialer.listen(listenerCtx, listenAddr)
	dialer.registerWG.Add(1)
	go dialer.registrationLoop(registerCtx)

	return dialer, nil
}

func (d *streamDialer) Close() error {
	d.closeOnce.Do(func() {
		d.listenerCancel()
		d.registerCancel()
		d.registerWG.Wait()
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
	d.callbackMu.RLock()
	callback := d.remoteDialogEnded
	d.callbackMu.RUnlock()
	if callback != nil {
		callback()
	}
}

// SetRemoteDialogEnded assigns the callback for the sole active stream.
func (d *streamDialer) SetRemoteDialogEnded(callback func()) {
	d.callbackMu.Lock()
	d.remoteDialogEnded = callback
	d.callbackMu.Unlock()
}

// Register announces this persistent Companion SIP endpoint to Flexisip.
// Registration failure is non-fatal so media startup can report its own failure.
func (d *streamDialer) Register(ctx context.Context) error {
	if d.client == nil || d.target.URI.Host == "" {
		return errors.New("sip: registration unavailable")
	}
	req := sip.NewRequest(sip.REGISTER, sip.Uri{Scheme: "sip", Host: d.target.URI.Host})
	req.SetTransport(strings.ToUpper(d.transport))
	req.AppendHeader(sip.NewHeader("To", fmt.Sprintf("<sip:%s@%s>", d.authUser, d.target.URI.Host)))
	req.AppendHeader(sip.NewHeader("From", fmt.Sprintf("<sip:%s@%s>;tag=%s", d.authUser, d.target.URI.Host, sip.GenerateTagN(16))))
	req.AppendHeader(sip.NewHeader("Contact", fmt.Sprintf("<sip:%s@%s:%d>", d.contact.Address.User, d.contact.Address.Host, d.contact.Address.Port)))
	req.AppendHeader(sip.NewHeader("Expires", "600"))
	response, err := d.client.Do(ctx, req, sipgo.ClientRequestRegisterBuild)
	if err != nil {
		return fmt.Errorf("send register: %w", err)
	}
	if response == nil || !response.IsSuccess() {
		if response == nil {
			return errors.New("sip: empty register response")
		}
		return fmt.Errorf("sip: register response status=%d", response.StatusCode)
	}
	d.logger.InfoContext(ctx, "sip registration succeeded", "domain", d.target.URI.Host)
	return nil
}

func (d *streamDialer) registrationLoop(ctx context.Context) {
	defer d.registerWG.Done()

	registrationLoop(ctx, registerRefreshInterval, registerCheckInterval, registerTimeout, d.Register)
}

func registrationLoop(ctx context.Context, refreshInterval, checkInterval, timeout time.Duration, register func(context.Context) error) {
	var lastSuccess time.Time
	tryRegister := func() {
		if !lastSuccess.IsZero() && time.Since(lastSuccess) < refreshInterval {
			return
		}

		registerCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if err := register(registerCtx); err == nil {
			lastSuccess = time.Now()
		}
	}

	tryRegister()
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tryRegister()
		}
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
	defer func() { _ = d.dialog.Close() }()
	if err := d.dialog.Bye(ctx); err != nil {
		return err
	}
	if err := waitForDialogEnd(ctx, d.dialog.Context().Done()); err != nil {
		return err
	}
	d.logger.InfoContext(ctx, "sip stream bye completed")
	return nil
}

func waitForDialogEnd(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
