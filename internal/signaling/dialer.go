package signaling

import (
	"bticino-go-companion/internal/core"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
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
	registerRetryMaximum    = 2 * time.Minute
)

var (
	ErrStreamTargetUnset = errors.New("sip: stream target not configured")

	// ErrDialogConcluded reports a response that was not sent because the
	// inbound dialog it belongs to had already been concluded by someone else.
	ErrDialogConcluded = errors.New("sip: inbound dialog already concluded")

	_ StreamDialer = (*streamDialer)(nil)
)

// StreamDialerConfig contains the outbound SIP settings required for a stream.
// Inbound handling is registered only when Inbound is set, so an installation
// without the Flexisip provisioning keeps behaving as an outbound-only endpoint.
type StreamDialerConfig struct {
	Target            string
	Domain            string
	From              string
	AuthUser          string
	AuthPass          string
	Transport         string
	Listen            string
	Inbound           bool
	Logger            *slog.Logger
	RemoteDialogEnded func()
}

type streamDialer struct {
	ua                *sipgo.UserAgent
	server            *sipgo.Server
	client            *sipgo.Client
	out               *sipgo.DialogClientCache
	in                *sipgo.DialogServerCache
	inboundMu         sync.RWMutex
	inbound           InboundHandler
	inboundClosing    bool
	inboundWG         sync.WaitGroup
	inboundDialogs    sync.Map
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
		return nil, errors.New("sip: invalid from address")
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

	// Every inbound handler is registered here, before the listener goroutine
	// exists, so nothing reads d.in while it is still being assigned.
	if cfg.Inbound {
		dialer.in = sipgo.NewDialogServerCache(client, contact)
		server.OnInvite(dialer.onInvite)
		server.OnCancel(dialer.onCancel)
		server.OnAck(dialer.onAck)
		dialer.logger.Info("inbound sip enabled")
	}

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

		d.inboundMu.Lock()
		d.inboundClosing = true
		d.inboundMu.Unlock()

		// The user agent goes down before the inbound handlers are waited for,
		// not after. An inbound handler parked on its INVITE transaction is
		// woken only by that transaction being terminated, which is what
		// ua.Close does, so waiting first would deadlock.
		d.closeErr = errors.Join(d.server.Close(), d.ua.Close())

		d.inboundWG.Wait()
	})

	return d.closeErr
}

// trackInbound registers a goroutine that may call into the inbound handler, so
// Close can wait for it, and reports whether it may run at all. It refuses once
// Close has begun: a request that races the shutdown must not add to a wait
// group that is already being waited on.
func (d *streamDialer) trackInbound() bool {
	d.inboundMu.Lock()
	defer d.inboundMu.Unlock()

	if d.inboundClosing {
		return false
	}

	d.inboundWG.Add(1)

	return true
}

func (d *streamDialer) listen(ctx context.Context, addr string) {
	d.logger.Info("sip listener started", "listen_addr", addr)

	if err := d.server.ListenAndServe(ctx, d.transport, addr); err != nil && ctx.Err() == nil {
		d.logger.Error("sip listener stopped", "listen_addr", addr, "error", err)
	}
}

// onBye terminates the dialog the BYE belongs to. The outbound stream is tried
// first because it is the only dialog an outbound-only installation can have;
// the inbound branch is reached only when this BYE matches no outbound dialog.
func (d *streamDialer) onBye(req *sip.Request, tx sip.ServerTransaction) {
	if err := d.out.ReadBye(req, tx); err == nil {
		d.logger.Info("remote sip stream ended")
		d.notifyRemoteDialogEnded()

		return
	}

	if d.in == nil {
		d.logger.Warn("remote sip bye rejected")

		return
	}

	d.onInboundBye(req, tx)
}

// onInboundBye ends an inbound dialog the far end hung up.
//
// The two cases are kept apart deliberately. A BYE that arrives while the call
// is still ringing has to reach the manager as the end of a pending call, or
// that call survives until the manager's own expiry and every stream request is
// refused meanwhile. A BYE for a call that was answered ends an *active* dialog
// instead, which is Manager.RemoteDialogEnded's job: without it the manager
// keeps believing a call is up, and StartStream deliberately returns nil
// without dialling while one is, so every later preview would succeed while
// sending no INVITE — a permanently black stream with no error anywhere.
//
// Two deviations from RFC 3261 are deliberate, and both follow from that
// constraint. A BYE on a dialog that was never established should be answered
// 481 Call/Transaction Does Not Exist; it is accepted instead, because refusing
// it would leave the ringing call pending. And sipgo's ReadBye terminates the
// INVITE transaction without putting a final response on it, so the caller sees
// its INVITE die unanswered; that is tolerated for the same reason. A far end
// that wants a clean 487 sends a CANCEL, which is the normal case.
func (d *streamDialer) onInboundBye(req *sip.Request, tx sip.ServerTransaction) {
	if !d.trackInbound() {
		d.logger.Warn("inbound sip bye dropped during shutdown")

		return
	}

	defer d.inboundWG.Done()

	session, err := d.in.MatchDialogRequest(req)
	if err != nil {
		d.logger.Warn("remote sip bye rejected", "error", err)

		return
	}

	// The state has to be read before ReadBye moves the dialog to Ended.
	state := session.LoadState()
	answered := state == sip.DialogStateEstablished || state == sip.DialogStateConfirmed
	dialog := d.loadInboundDialog(session.ID)

	if err := session.ReadBye(req, tx); err != nil {
		d.logger.Warn("inbound sip bye rejected", "error", err)

		return
	}

	// endPendingIncoming is called for its effect, so it stays out of the
	// branch conditions of a switch, where the reader would have to know the
	// evaluation order to know whether it runs at all.
	if dialog != nil && d.endPendingIncoming(dialog, cancelReason(req), "bye") {
		return
	}

	if answered {
		// Manager.RemoteDialogEnded clears the active call without sending a BYE
		// back, because the far end has already sent one.
		//
		// It is reached through the inbound handler this dialer already holds,
		// and deliberately not through the remoteDialogEnded callback: that one
		// is the outbound stream hook, owned by the media source, and means the
		// preview's own dialog went away. Calling it here would tear down media
		// for a call the user is still on.
		//
		// The call is synchronous, which is safe because this goroutine is
		// already tracked by inboundWG and RemoteDialogEnded neither blocks nor
		// re-enters the dialer: it takes the manager's state lock, clears the
		// active dialog and queues the event for the manager's own drain
		// goroutine.
		if handler := d.inboundHandler(); handler != nil {
			handler.RemoteDialogEnded()
		}

		d.logger.Info("inbound sip call ended by remote", "dialog_id", inboundDialogID(dialog))

		return
	}

	d.logger.Debug("inbound sip bye ignored", "dialog_id", inboundDialogID(dialog))
}

func (d *streamDialer) notifyRemoteDialogEnded() {
	d.callbackMu.RLock()
	callback := d.remoteDialogEnded
	d.callbackMu.RUnlock()

	if callback != nil {
		go callback()
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

	return nil
}

func (d *streamDialer) registrationLoop(ctx context.Context) {
	defer d.registerWG.Done()

	delay := time.Duration(0)
	failed := false

	for {
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}

		registerCtx, cancel := context.WithTimeout(ctx, registerTimeout)
		err := d.Register(registerCtx)

		cancel()

		if err != nil {
			if delay == 0 {
				delay = registerCheckInterval
			} else {
				delay = min(delay*2, registerRetryMaximum)
			}

			retryIn := jitter(delay)
			d.logger.Warn("sip registration failed", "error", err, "retry_in", retryIn)

			failed = true
			delay = retryIn

			continue
		}

		if failed {
			d.logger.Info("sip registration recovered")
		} else {
			d.logger.Debug("sip registration succeeded")
		}

		failed = false
		delay = registerRefreshInterval
	}
}

func jitter(delay time.Duration) time.Duration {
	percent := int64(100)
	if value, err := rand.Int(rand.Reader, big.NewInt(41)); err == nil {
		percent = 80 + value.Int64()
	}

	return min(delay*time.Duration(percent)/100, registerRetryMaximum)
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

func (d *streamDialer) StartStream(ctx context.Context, _, offer string) (OutgoingDialog, error) {
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

// InboundHandler receives inbound dialog lifecycle events from the SIP server.
//
// The two termination methods are not interchangeable. EndIncoming clears a
// call that is still pending, without matching a dialog identity;
// RemoteDialogEnded clears one that was answered here and has since been torn
// down by the far end. Sending either down the other's path drops the wrong
// call — see onInboundBye, which is what discriminates them.
//
// Every method is called from a goroutine Close waits for, so an implementation
// must not block on anything that only shutdown can release, and in particular
// must never wait on the dialer itself.
type InboundHandler interface {
	OnInvite(context.Context, IncomingDialog) error
	EndIncoming(core.CallEndReason)
	RemoteDialogEnded()
}

// SetInboundHandler assigns the component that decides how inbound calls are
// answered. Inbound requests are rejected until it is set.
func (d *streamDialer) SetInboundHandler(handler InboundHandler) {
	d.inboundMu.Lock()
	d.inbound = handler
	d.inboundMu.Unlock()
}

func (d *streamDialer) inboundHandler() InboundHandler {
	d.inboundMu.RLock()
	defer d.inboundMu.RUnlock()

	return d.inbound
}

// onInvite serves an inbound INVITE until the call is either answered or over.
//
// It deliberately does not return while the call is only ringing. sipgo
// terminates the INVITE server transaction as soon as this handler returns —
// Server.handleRequest calls tx.TerminateGracefully, which on a reliable
// transport terminates outright — and a terminated transaction can no longer
// carry the 200 OK of an answer that arrives seconds later, when the user
// finally picks up.
//
// It does not, however, live for the whole life of an answered dialog, and must
// not be read as doing so. After a 2xx the INVITE transaction enters RFC 6026's
// Accepted state and arms Timer_L = 64*T1 = 32s; when that fires the transaction
// is deleted and tx.Done() closes. So this handler returns 32 seconds into every
// answered call, and an answered dialog outlives it: the teardown below runs
// only for a dialog that is actually over, and a live one stays in the inbound
// map and in sipgo's server cache until a BYE, a Hangup or the manager ends it.
func (d *streamDialer) onInvite(req *sip.Request, tx sip.ServerTransaction) {
	handler := d.inboundHandler()
	if handler == nil || d.in == nil {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 503, "Service Unavailable", nil))

		return
	}

	// Close waits for this goroutine, because it can still be inside
	// handler.OnInvite — publishing events into the manager — long after the
	// listener has been told to stop.
	if !d.trackInbound() {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 503, "Service Unavailable", nil))

		return
	}

	defer d.inboundWG.Done()

	session, err := d.in.ReadInvite(req, tx)
	if err != nil {
		d.logger.Warn("inbound invite rejected", "error", err)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 500, "Server Error", nil))

		return
	}

	dialogID := core.DialogID("")
	if callID := req.CallID(); callID != nil {
		dialogID = core.DialogID(callID.Value())
	}

	dialog := &incomingDialog{session: session, id: dialogID, logger: d.logger}

	d.inboundDialogs.Store(session.ID, dialog)

	// The map entry lives exactly as long as the dialog, which is why this
	// handler cannot own it: the handler returns 32 seconds into an answered
	// call while the dialog goes on. Every way a dialog ends — a CANCEL, a
	// rejection, a BYE from either side, a local hangup — drives it to Ended,
	// so this fires exactly once and never for a call that is still up.
	session.OnState(func(state sip.DialogState) {
		if state == sip.DialogStateEnded {
			d.inboundDialogs.Delete(session.ID)
		}
	})

	// Closing is the only thing that drops the session from sipgo's server
	// cache — DialogServerCache.ReadInvite is what installs the onClose that
	// deletes it — and nothing in the library does it for a CANCEL: the
	// transaction layer emits the 487 itself and sipgo's own hook only ends the
	// dialog. Without this, every cancelled call leaks its session, cloned
	// INVITE and context for the life of the process, and a doorbell call
	// forked to several handsets and answered on one of them takes exactly that
	// path.
	//
	// The one dialog that must survive is a live answered one, which is why
	// this is not unconditional. Everything else that gets here is over: an
	// unanswered dialog whose transaction died is dead whether or not sipgo has
	// marked it Ended yet — ServerTx closes its done channel before it calls
	// the hook that ends the dialog, so tx.Done() can win that race.
	defer func() {
		switch session.LoadState() {
		case sip.DialogStateEstablished, sip.DialogStateConfirmed:
			d.logger.Debug("inbound sip invite transaction retired", "dialog_id", string(dialogID))

			return
		}

		_ = session.Close()
		d.inboundDialogs.Delete(session.ID)
		d.logger.Debug("inbound sip dialog closed", "dialog_id", string(dialogID))
	}()

	// A CANCEL that matches this INVITE is answered, turned into a 487 and
	// reported through this hook by the transaction layer itself: it never
	// reaches the server's CANCEL handler. Registering the hook before the
	// manager is told about the call keeps a CANCEL that races the 180 Ringing
	// from being lost.
	if !tx.OnCancel(func(cancel *sip.Request) {
		// The transaction must not be blocked in here, hence the goroutine —
		// which Close waits for too, because it ends up in the manager.
		if !d.trackInbound() {
			return
		}

		go func() {
			defer d.inboundWG.Done()

			d.endPendingIncoming(dialog, cancelReason(cancel), "cancel")
		}()
	}) {
		// A false answer means this INVITE is already dead: it was cancelled or
		// terminated in the few instructions between ReadInvite, which
		// registers a hook of its own, and this one. The manager must not hear
		// about it at all. Telling it would reserve the incoming slot and
		// publish IncomingCallStarted for a call that can no longer be rung,
		// and the rollback that follows reports it as cancelled — misreporting
		// a call answered on another handset, whose CANCEL carried cause=200.
		d.logger.Warn("inbound sip invite already terminated", "dialog_id", string(dialogID))

		return
	}

	d.logger.Info("inbound sip invite received", "dialog_id", string(dialogID))

	if err := handler.OnInvite(context.Background(), dialog); err != nil {
		d.logger.Warn("inbound invite handling failed", "dialog_id", string(dialogID), "error", err)

		return
	}

	// Both are needed: the dialog context ends on a BYE, a rejection, a CANCEL
	// or an answered call being hung up, and the transaction ends on its own —
	// when it times out with the far end silent, and 32 seconds after every
	// answer, when Timer_L retires it.
	select {
	case <-session.Context().Done():
	case <-tx.Done():
	}
}

// onCancel answers a CANCEL the transaction layer could not match to an INVITE
// transaction of ours. A matching one never gets here: it is answered and
// turned into a 487 by the transaction layer, which reports it through the
// transaction's own cancel hook instead. So this CANCEL refers by construction
// to a transaction that no longer exists, and it must not end the pending call
// — Manager.EndIncoming clears whatever call is pending, without matching a
// dialog identity, so a speculative call here would drop an unrelated call.
func (d *streamDialer) onCancel(req *sip.Request, tx sip.ServerTransaction) {
	d.logger.Info("unmatched inbound sip cancel", "reason", string(cancelReason(req)))

	_ = tx.Respond(sip.NewResponseFromRequest(req, 481, "Call/Transaction Does Not Exist", nil))
}

func (d *streamDialer) onAck(req *sip.Request, tx sip.ServerTransaction) {
	if d.in == nil {
		return
	}

	if err := d.in.ReadAck(req, tx); err != nil {
		d.logger.Debug("inbound sip ack ignored", "error", err)
	}
}

// endPendingIncoming tells the inbound handler that a call ended before it was
// answered, and reports whether it did.
//
// The dialog is claimed first because Manager.EndIncoming clears whatever call
// is pending without matching a dialog: claiming is what keeps a CANCEL or a
// BYE for a rejected, already answered or already terminated INVITE from
// clearing the call that is genuinely ringing.
func (d *streamDialer) endPendingIncoming(dialog *incomingDialog, reason core.CallEndReason, cause string) bool {
	handler := d.inboundHandler()
	if handler == nil {
		return false
	}

	if !dialog.endPending() {
		return false
	}

	d.logger.Info("inbound sip call ended before answer",
		"dialog_id", string(dialog.ID()),
		"cause", cause,
		"reason", string(reason),
	)
	handler.EndIncoming(reason)

	return true
}

func (d *streamDialer) loadInboundDialog(id string) *incomingDialog {
	value, ok := d.inboundDialogs.Load(id)
	if !ok {
		return nil
	}

	dialog, _ := value.(*incomingDialog)

	return dialog
}

func inboundDialogID(dialog *incomingDialog) string {
	if dialog == nil {
		return ""
	}

	return string(dialog.ID())
}

// serverSession is the part of sipgo.DialogServerSession the adapter uses.
type serverSession interface {
	Respond(int, string, []byte, ...sip.Header) error
	RespondSDP([]byte) error
	Bye(context.Context) error
	Close() error
	LoadState() sip.DialogState
}

// incomingDialog adapts a sipgo server session to the signaling interface.
//
// sipgo does not serialize responses on a session: DialogServerSession.Respond
// stores the response on the dialog and drives the invite transaction with no
// lock of its own, and for a 200 OK it stays in there retransmitting until the
// ACK arrives. The manager does respond concurrently on one dialog — the expiry
// timer and an in-flight Answer overlap in a narrow window — so the adapter is
// what keeps those responses apart.
type incomingDialog struct {
	session serverSession
	id      core.DialogID
	logger  *slog.Logger

	// mu guards the two fields below and is held across a provisional response,
	// so a final one can never start writing while a 180 is still on the wire.
	mu        sync.Mutex
	ringing   bool
	concluded bool
}

var _ IncomingDialog = (*incomingDialog)(nil)

func (d *incomingDialog) ID() core.DialogID { return d.id }

// Respond sends a provisional or final response.
//
// What a response is follows from its status alone: below 200 provisional,
// 2xx the answer, anything above a rejection. Discriminating on the body
// instead would send a bodyless 200 OK down the rejection path — closing and
// dropping from sipgo's server cache the very dialog that response has just
// established, after which no BYE for it can be matched — and would send a
// provisional carrying early SDP as an answer.
//
// Only the first final response is sent. A later one is refused with
// ErrDialogConcluded rather than dropped silently, so a caller that believes it
// answered the call — and would go on to set up media for it — is told that
// somebody else concluded the dialog first.
//
// ctx bounds a final response, which is the only kind that waits for the far
// end: a provisional one is written and done. See answer and reject for what
// giving up on that wait does and does not mean.
func (d *incomingDialog) Respond(ctx context.Context, status int, reason, body string) error {
	var payload []byte
	if body != "" {
		payload = []byte(body)
	}

	d.mu.Lock()

	if d.concluded {
		d.mu.Unlock()

		return ErrDialogConcluded
	}

	if status < 200 {
		defer d.mu.Unlock()

		if err := d.session.Respond(status, reason, payload); err != nil {
			return fmt.Errorf("sip provisional response: %w", err)
		}

		d.ringing = true

		return nil
	}

	// The final response is claimed under the lock and written with it
	// released: writing it waits for the far end, and nothing else may be
	// written on this dialog afterwards anyway.
	d.concluded = true
	d.mu.Unlock()

	if status < 300 {
		return d.answer(ctx, status, reason, payload)
	}

	return d.reject(ctx, status, reason)
}

// answer sends the 2xx and stops waiting when ctx expires, which is not the same
// as abandoning the answer.
//
// sipgo takes no context here: DialogServerSession.WriteResponse hands the 200 OK
// to the INVITE transaction and then stays in a retransmit loop until the ACK
// arrives or Timer_L retires the transaction, 64*T1 = 32s later. Waiting all of
// that out is what made an answered call report a failure: the caller's own
// deadline expired long before, and the error that finally came back was reported
// for a call the intercom had already connected. A card told its answer failed
// never starts media for it — and never shows the button that would hang it up.
//
// So expiry is reported as success, but only against evidence. WriteResponse
// stores the response on the dialog and marks it Established before it responds
// on the transaction, so an Established dialog is one whose answer sipgo has
// already committed to sending. That state is stored atomically, which is also
// what publishes the stored response to whoever runs next: sipgo documents
// Dialog.InviteResponse as not thread safe, and Bye reads it. Without the
// evidence there is nothing to report but the wait itself, so the caller waits.
//
// The write is left to finish either way. It is the only thing that retransmits
// the answer, and cutting it short would abandon the ACK the moment a caller
// stopped watching for it.
func (d *incomingDialog) answer(ctx context.Context, status int, reason string, payload []byte) error {
	done := make(chan error, 1)

	go func() { done <- d.writeAnswer(status, reason, payload) }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}

	if d.session.LoadState() < sip.DialogStateEstablished {
		return <-done
	}

	d.reportAbandoned(done, "inbound sip answer left unacknowledged")

	return nil
}

func (d *incomingDialog) writeAnswer(status int, reason string, payload []byte) error {
	// RespondSDP builds the Content-Type and Content-Length an SDP answer needs,
	// and refuses a nil body, so a bodyless 2xx goes out plain.
	if payload == nil {
		if err := d.session.Respond(status, reason, nil); err != nil {
			return fmt.Errorf("sip answer response: %w", err)
		}

		return nil
	}

	if err := d.session.RespondSDP(payload); err != nil {
		return fmt.Errorf("sip answer response: %w", err)
	}

	return nil
}

// reject sends a final response above 2xx and, like answer, stops waiting when
// ctx expires. What expiry means here is weaker, and does not need evidence.
//
// sipgo passes a rejection to the transaction and then waits for its ACK purely
// for a cleaner exit: that wait ends either way — with the ACK, or with the
// transaction timing out 32s later — and WriteResponse reports success for both,
// so a caller that stops waiting gives up on nothing it would have been told
// about. The rejection is also the last thing that happens on this dialog: it
// concluded it, and the write closes it, so no later caller can race the part
// that is still finishing.
func (d *incomingDialog) reject(ctx context.Context, status int, reason string) error {
	done := make(chan error, 1)

	go func() { done <- d.writeRejection(status, reason) }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}

	d.reportAbandoned(done, "inbound sip rejection left unacknowledged")

	return nil
}

func (d *incomingDialog) writeRejection(status int, reason string) error {
	// A rejection is the end of the dialog, and closing it drops the session
	// from the server cache, which would otherwise hold it for the lifetime of
	// the process. It runs before the result is handed back, so a caller that
	// sees the write finish sees a closed session too.
	defer func() { _ = d.session.Close() }()

	if err := d.session.Respond(status, reason, nil); err != nil {
		return fmt.Errorf("sip final response: %w", err)
	}

	return nil
}

// reportAbandoned logs how a final response ended once nobody is waiting for it
// any more. Without this the one thing that says the far end never acknowledged
// a response — on hardware that cannot be inspected — would be discarded.
func (d *incomingDialog) reportAbandoned(done <-chan error, message string) {
	logger := d.logger
	if logger == nil {
		logger = slog.Default()
	}

	go func() {
		if err := <-done; err != nil {
			logger.Warn(message, "dialog_id", string(d.id), "error", err)
		}
	}()
}

func (d *incomingDialog) Bye(ctx context.Context) error {
	d.mu.Lock()
	d.concluded = true
	d.mu.Unlock()

	defer func() { _ = d.session.Close() }()

	// sipgo can only BYE a dialog it has answered: it dereferences the stored
	// invite response, which is nil until one was sent. Closing is all there is
	// to do for a dialog that never reached 200 OK.
	if d.session.LoadState() < sip.DialogStateEstablished {
		return nil
	}

	if err := d.session.Bye(ctx); err != nil {
		return fmt.Errorf("sip inbound bye: %w", err)
	}

	return nil
}

// endPending claims the dialog on behalf of a CANCEL or a BYE that arrived
// while the call was still ringing. It answers true exactly once, and only for
// the dialog that is actually pending: one that has rung and that no answer, no
// rejection and no earlier termination has concluded.
func (d *incomingDialog) endPending() bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.ringing || d.concluded {
		return false
	}

	d.concluded = true

	return true
}

// cancelReason reads why the far end withdrew the call. Flexisip forks a
// doorbell call to every registered companion and cancels the losers with
// cause=200 once one of them answers, which is not the visitor giving up.
func cancelReason(req *sip.Request) core.CallEndReason {
	header := req.GetHeader("Reason")
	if header != nil && strings.Contains(header.Value(), "cause=200") {
		return core.CallEndReasonElsewhere
	}

	return core.CallEndReasonCancelled
}
