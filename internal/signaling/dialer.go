package signaling

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

const streamAnswerTimeout = 8 * time.Second

var (
	ErrUnsupportedStreamModel              = errors.New("sip: unsupported stream model")
	ErrStreamTargetUnset                   = errors.New("sip: stream target not configured")
	_                         StreamDialer = (*streamDialer)(nil)
)

// StreamDialerConfig contains the outbound SIP settings required for a stream.
// Registration and inbound SIP handling are deliberately outside this dialer.
type StreamDialerConfig struct {
	Model     string
	Target    string
	Domain    string
	From      string
	AuthUser  string
	AuthPass  string
	Transport string
	Listen    string
}

// StreamProfile contains SIP behavior verified for a supported intercom model.
type StreamProfile struct {
	Model         string
	DefaultTarget string
}

func ResolveStreamProfile(model string) (StreamProfile, error) {
	switch strings.ToUpper(strings.TrimSpace(model)) {
	case "C300X":
		return StreamProfile{Model: "C300X", DefaultTarget: "c300x@127.0.0.1"}, nil
	case "C100X":
		return StreamProfile{Model: "C100X", DefaultTarget: "c100x@127.0.0.1"}, nil
	default:
		return StreamProfile{}, ErrUnsupportedStreamModel
	}
}

type streamDialer struct {
	ua        *sipgo.UserAgent
	out       *sipgo.DialogClientCache
	target    inviteTarget
	authUser  string
	authPass  string
	transport string
}

func NewStreamDialer(cfg StreamDialerConfig) (*streamDialer, error) {
	profile, err := ResolveStreamProfile(cfg.Model)
	if err != nil {
		return nil, err
	}

	target, err := resolveInviteTarget(firstNonEmpty(cfg.Target, profile.DefaultTarget), cfg.Domain)
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

	contact := sip.ContactHeader{Address: sip.Uri{User: fromUser, Host: fromHost, Port: fromPort}}
	return &streamDialer{
		ua:        ua,
		out:       sipgo.NewDialogClientCache(client, contact),
		target:    target,
		authUser:  firstNonEmpty(cfg.AuthUser, fromUser),
		authPass:  strings.TrimSpace(cfg.AuthPass),
		transport: normalizeTransport(cfg.Transport),
	}, nil
}

func (d *streamDialer) Close() error {
	return d.ua.Close()
}

func (d *streamDialer) StartStream(ctx context.Context, _ string, offer string) (OutgoingDialog, error) {
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

	return outgoingDialog{dialog: dialog}, nil
}

type outgoingDialog struct {
	dialog *sipgo.DialogClientSession
}

func (d outgoingDialog) Bye(ctx context.Context) error {
	err := d.dialog.Bye(ctx)
	_ = d.dialog.Close()
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
