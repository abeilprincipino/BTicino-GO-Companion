package sipprotocol

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/emiago/sipgo/sip"
)

var ErrTargetUnset = errors.New("sip target not configured")

type InviteTarget struct {
	URI         sip.Uri
	Destination string
	AddDevAddr  bool
}

func ResolveInviteTarget(rawTarget string, domain string, addDevAddr bool) (InviteTarget, error) {
	target := strings.TrimSpace(rawTarget)
	if target == "" {
		return InviteTarget{}, ErrTargetUnset
	}

	hadAt := strings.Contains(target, "@")
	raw := target
	if !strings.HasPrefix(strings.ToLower(raw), "sip:") && !strings.HasPrefix(strings.ToLower(raw), "sips:") {
		raw = "sip:" + raw
	}

	var uri sip.Uri
	if err := sip.ParseUri(raw, &uri); err != nil {
		return InviteTarget{}, fmt.Errorf("parse sip target: %w", err)
	}

	destination := hostPortFromURI(uri)
	domain = strings.TrimSpace(domain)
	if !hadAt && strings.TrimSpace(uri.User) == "" && strings.TrimSpace(uri.Host) != "" && domain != "" {
		uri.User = uri.Host
		uri.Host = domain
	}

	if domain != "" && hostIsIPAddressOrLocal(uri.Host) {
		if destination == "" {
			destination = hostPortFromURI(uri)
		}
		uri.Host = domain
	}

	if strings.TrimSpace(uri.Host) == "" {
		if domain == "" {
			return InviteTarget{}, fmt.Errorf("%w: missing host/domain", ErrTargetUnset)
		}
		uri.Host = domain
	}
	if strings.TrimSpace(uri.User) == "" {
		return InviteTarget{}, fmt.Errorf("%w: missing user", ErrTargetUnset)
	}

	if destination == "" && hostIsIPAddressOrLocal(uri.Host) {
		destination = hostPortFromURI(uri)
	}

	return InviteTarget{
		URI:         uri,
		Destination: destination,
		AddDevAddr:  addDevAddr,
	}, nil
}

func ParseFromAddress(raw string) (string, string, int) {
	val := strings.TrimSpace(raw)
	if strings.HasPrefix(strings.ToLower(val), "sip:") {
		val = val[4:]
	}
	val = strings.SplitN(val, ";", 2)[0]
	if val == "" {
		return "", "", 0
	}

	parts := strings.SplitN(val, "@", 2)
	if len(parts) != 2 {
		return "", "", 0
	}
	user := strings.TrimSpace(parts[0])
	hostPart := strings.TrimSpace(parts[1])
	host := hostPart
	port := 0

	if strings.Contains(hostPart, ":") {
		if h, p, err := net.SplitHostPort(hostPart); err == nil {
			host = strings.TrimSpace(h)
			port, _ = strconv.Atoi(strings.TrimSpace(p))
		}
	}
	return user, host, port
}

func hostPortFromURI(uri sip.Uri) string {
	host := strings.TrimSpace(uri.Host)
	if host == "" {
		return ""
	}
	if uri.Port > 0 {
		return net.JoinHostPort(host, strconv.Itoa(uri.Port))
	}
	return net.JoinHostPort(host, "5060")
}

func hostIsIPAddressOrLocal(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return false
	}
	if h == "localhost" || h == "127.0.0.1" || h == "0.0.0.0" {
		return true
	}
	return net.ParseIP(h) != nil
}
