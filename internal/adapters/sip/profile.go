package sipadapter

import (
	"bufio"
	"errors"
	"os"
	"strings"

	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/protocol/sip"
)

var (
	flexisipDomainRegistrationPaths = []string{
		"/etc/flexisip/domain-registration.conf",
	}
	flexisipConfigPaths = []string{
		"/etc/flexisip/flexisip.conf",
		"/home/bticino/cfg/flexisip.conf",
	}
	flexisipUsersDBPaths = []string{
		"/etc/flexisip/users/users.db.txt",
	}
)

var ErrSIPProfileUnset = errors.New("sip runtime profile not configured")

type flexisipProfile struct {
	Domain   string
	AuthUser string
}

func resolveSIPConfig(cfg config.Config) (config.Config, error) {
	out := cfg
	profile := discoverFlexisipProfile()

	domain := strings.TrimSpace(out.MediaSIPDomain)
	if domain == "" {
		domain = profile.Domain
		out.MediaSIPDomain = domain
	}

	from := strings.TrimSpace(out.MediaSIPFrom)
	if (from == "" || strings.EqualFold(from, "webrtc@127.0.0.1") || strings.EqualFold(from, "sip:webrtc@127.0.0.1")) && profile.AuthUser != "" {
		out.MediaSIPFrom = profile.AuthUser + "@127.0.0.1"
	}

	if strings.TrimSpace(out.MediaSIPAuthUser) == "" && profile.AuthUser != "" {
		out.MediaSIPAuthUser = profile.AuthUser
	}

	to := strings.TrimSpace(out.MediaSIPTo)
	if to == "" {
		if defaultTarget, ok := defaultSIPTargetFromModel(out.DeviceModel); ok {
			out.MediaSIPTo = defaultTarget
		}
	}

	fromUser, _, _ := sipprotocol.ParseFromAddress(out.MediaSIPFrom)
	if strings.TrimSpace(fromUser) == "" {
		return cfg, ErrSIPProfileUnset
	}
	return out, nil
}

func discoverFlexisipProfile() flexisipProfile {
	profile := flexisipProfile{}
	profile.Domain = discoverFlexisipDomain()
	profile.AuthUser = discoverFlexisipAuthUser(profile.Domain)
	return profile
}

func discoverFlexisipDomain() string {
	if body, ok := readFirstExistingFile(flexisipDomainRegistrationPaths); ok {
		for _, line := range splitNonEmptyLines(body) {
			fields := strings.Fields(line)
			if len(fields) > 0 {
				return strings.TrimSpace(fields[0])
			}
		}
	}

	for _, path := range flexisipConfigPaths {
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range splitNonEmptyLines(string(body)) {
			if val, ok := parseKVLine(line, "aliases"); ok {
				return val
			}
			if val, ok := parseKVLine(line, "reg-domains"); ok {
				return val
			}
			if val, ok := parseKVLine(line, "auth-domains"); ok {
				return val
			}
		}
	}
	return ""
}

func discoverFlexisipAuthUser(domain string) string {
	body, ok := readFirstExistingFile(flexisipUsersDBPaths)
	if !ok {
		return ""
	}
	for _, line := range splitNonEmptyLines(body) {
		if strings.HasPrefix(line, "version:") {
			continue
		}
		token := strings.Fields(line)
		if len(token) == 0 {
			continue
		}
		user, host, ok := parseUserAtHost(token[0])
		if !ok || user == "" {
			continue
		}
		if domain == "" || strings.EqualFold(host, domain) {
			return user
		}
	}
	return ""
}

func parseUserAtHost(value string) (string, string, bool) {
	v := strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(v), "sip:") {
		v = v[4:]
	}
	parts := strings.SplitN(v, "@", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	user := strings.TrimSpace(parts[0])
	host := strings.TrimSpace(parts[1])
	host = strings.SplitN(host, ";", 2)[0]
	if user == "" || host == "" {
		return "", "", false
	}
	return user, host, true
}

func parseKVLine(line string, key string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", false
	}
	prefix := key + "="
	if !strings.HasPrefix(trimmed, prefix) {
		return "", false
	}
	val := strings.TrimSpace(strings.TrimPrefix(trimmed, prefix))
	if val == "" {
		return "", false
	}
	if strings.Contains(val, " ") {
		val = strings.Fields(val)[0]
	}
	return val, true
}

func readFirstExistingFile(paths []string) (string, bool) {
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err == nil {
			return string(body), true
		}
	}
	return "", false
}

func splitNonEmptyLines(content string) []string {
	lines := make([]string, 0)
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

func defaultSIPTargetFromModel(deviceModel string) (string, bool) {
	model := strings.ToLower(strings.TrimSpace(deviceModel))
	switch model {
	case "c300x", "c100x":
		return model + "@127.0.0.1", true
	default:
		return "", false
	}
}
