package signaling

import (
	"bufio"
	"os"
	"strings"
)

var (
	flexisipDomainRegistrationPaths = []string{
		"/etc/flexisip/domain-registration.conf",
	}
	flexisipConfigPaths = []string{
		"/etc/flexisip/flexisip.conf",
		"/home/bticino/cfg/flexisip.conf",
	}
)

// DiscoverFlexisipDomain returns the local Flexisip domain using the same
// file paths and precedence as the V2 companion.
func DiscoverFlexisipDomain() string {
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
