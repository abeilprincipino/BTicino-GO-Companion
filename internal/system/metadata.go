package system

import (
	"bufio"
	"encoding/xml"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

type NetworkSnapshot struct {
	Interface string
	IP        string
	MAC       string
	WiFiRSSI  *int
}

type LocalMetadata struct {
	Model    string
	Firmware string
	Network  NetworkSnapshot
}

func DetectLocalMetadata() LocalMetadata {
	return LocalMetadata{
		Model:    DetectDeviceModel(),
		Firmware: detectFirmwareVersion(),
		Network:  detectNetworkSnapshot(),
	}
}

func DetectDeviceModel() string {
	for _, path := range []string{
		"/etc/opkg/base-feeds.conf",
		"/etc/opkg/arch.conf",
		"/home/bticino/sp/dbfiles_ws.xml",
	} {
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if model, ok := parseModel(string(content)); ok {
			return model
		}
	}
	return "unknown"
}

func DetectDeviceMAC() string {
	snap := detectNetworkSnapshot()
	if strings.TrimSpace(snap.MAC) == "" {
		return ""
	}
	return snap.MAC
}

func detectFirmwareVersion() string {
	content, err := os.ReadFile("/home/bticino/sp/dbfiles_ws.xml")
	if err != nil {
		return ""
	}
	version, ok := parseFirmwareVersion(string(content))
	if !ok {
		return ""
	}
	return version
}

func detectNetworkSnapshot() NetworkSnapshot {
	snap, ok := detectNetworkSnapshotViaConnMan()
	if !ok {
		return NetworkSnapshot{}
	}
	return snap
}

func detectNetworkSnapshotViaConnMan() (NetworkSnapshot, bool) {
	out, err := exec.Command(
		"/usr/bin/dbus-send",
		"--system",
		"--print-reply",
		"--dest=net.connman",
		"/",
		"net.connman.Manager.GetServices",
	).Output()
	if err != nil {
		return NetworkSnapshot{}, false
	}

	services := splitConnManServiceBlocks(string(out))
	if len(services) == 0 {
		return NetworkSnapshot{}, false
	}

	best := connManService{score: -1}
	for _, block := range services {
		svc := parseConnManServiceBlock(block)
		if !strings.EqualFold(strings.TrimSpace(svc.Type), "wifi") {
			continue
		}
		if strings.TrimSpace(svc.Interface) == "" || strings.TrimSpace(svc.IP) == "" {
			continue
		}
		score := connManServiceScore(svc.State)
		svc.score = score
		if svc.score > best.score {
			best = svc
		}
	}

	if best.score < 0 {
		return NetworkSnapshot{}, false
	}

	snap := NetworkSnapshot{
		Interface: strings.TrimSpace(best.Interface),
		IP:        strings.TrimSpace(best.IP),
		MAC:       normalizeMACString(best.MAC),
	}
	if best.Strength != nil {
		v := *best.Strength
		if v < 0 {
			v = 0
		}
		if v > 100 {
			v = 100
		}
		snap.WiFiRSSI = &v
	}
	return snap, true
}

type connManService struct {
	Type      string
	State     string
	Interface string
	IP        string
	MAC       string
	Strength  *int
	score     int
}

func splitConnManServiceBlocks(output string) []string {
	lines := strings.Split(output, "\n")
	blocks := make([]string, 0, 4)
	depth := 0
	var current strings.Builder

	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if strings.Contains(line, "struct {") && depth == 0 {
			depth = 1
			current.Reset()
			current.WriteString(raw)
			current.WriteByte('\n')
			continue
		}
		if depth == 0 {
			continue
		}

		openCount := strings.Count(line, "{")
		closeCount := strings.Count(line, "}")
		if !(openCount == 1 && strings.Contains(line, "struct {")) {
			depth += openCount
		}
		depth -= closeCount
		current.WriteString(raw)
		current.WriteByte('\n')
		if depth <= 0 {
			blocks = append(blocks, current.String())
			depth = 0
		}
	}
	return blocks
}

func parseConnManServiceBlock(block string) connManService {
	service := connManService{}
	section := "top"
	currentKey := ""

	scanner := bufio.NewScanner(strings.NewReader(block))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		if key, ok := parseDBusKey(line); ok {
			currentKey = key
			switch key {
			case "Ethernet":
				section = "ethernet"
				currentKey = ""
			case "IPv4":
				section = "ipv4"
				currentKey = ""
			case "IPv6", "Proxy", "Provider":
				section = "other"
				currentKey = ""
			}
			continue
		}

		if !strings.Contains(line, "variant") || currentKey == "" {
			continue
		}

		value, ok := parseDBusVariantValue(line)
		if !ok {
			continue
		}

		switch section {
		case "top":
			switch currentKey {
			case "Type":
				service.Type = value
			case "State":
				service.State = value
			case "Strength":
				if n, err := strconv.Atoi(value); err == nil {
					v := n
					service.Strength = &v
				}
			}
		case "ethernet":
			switch currentKey {
			case "Interface":
				service.Interface = value
			case "Address":
				service.MAC = value
			}
		case "ipv4":
			if currentKey == "Address" {
				service.IP = value
			}
		}
	}

	return service
}

func parseDBusKey(line string) (string, bool) {
	if !strings.HasPrefix(line, "string \"") {
		return "", false
	}
	start := strings.Index(line, "\"")
	if start < 0 {
		return "", false
	}
	rest := line[start+1:]
	end := strings.Index(rest, "\"")
	if end < 0 {
		return "", false
	}
	key := strings.TrimSpace(rest[:end])
	if key == "" {
		return "", false
	}
	return key, true
}

func parseDBusVariantValue(line string) (string, bool) {
	parts := strings.Fields(line)
	if len(parts) < 3 {
		return "", false
	}
	for i := 0; i < len(parts); i++ {
		if parts[i] != "variant" {
			continue
		}
		if i+1 >= len(parts) {
			return "", false
		}
		typeName := strings.TrimSpace(parts[i+1])
		switch typeName {
		case "string":
			if idx := strings.Index(line, "\""); idx >= 0 {
				rest := line[idx+1:]
				if end := strings.LastIndex(rest, "\""); end >= 0 {
					return strings.TrimSpace(rest[:end]), true
				}
			}
			return "", false
		case "byte", "int16", "int32", "uint16", "uint32", "int64", "uint64":
			if i+2 >= len(parts) {
				return "", false
			}
			return strings.TrimSpace(parts[i+2]), true
		default:
			return "", false
		}
	}
	return "", false
}

func connManServiceScore(state string) int {
	s := strings.ToLower(strings.TrimSpace(state))
	switch s {
	case "online", "ready":
		return 30
	case "association", "configuration":
		return 20
	case "idle":
		return 10
	default:
		return 5
	}
}

func parseModel(content string) (string, bool) {
	upper := strings.ToUpper(content)
	switch {
	case strings.Contains(upper, "C300X"):
		return "C300X", true
	case strings.Contains(upper, "C100X"):
		return "C100X", true
	default:
		return "", false
	}
}

type dbfilesWS struct {
	VerWebserver string `xml:"general_info>ver_webserver"`
}

var firmwareTagRegex = regexp.MustCompile(`(?is)<ver_webserver>\s*([^<]+?)\s*</ver_webserver>`)

func parseFirmwareVersion(content string) (string, bool) {
	var parsed dbfilesWS
	if err := xml.Unmarshal([]byte(content), &parsed); err != nil {
		if !strings.Contains(err.Error(), "CharsetReader is nil") {
			return "", false
		}
		matches := firmwareTagRegex.FindStringSubmatch(content)
		if len(matches) < 2 {
			return "", false
		}
		version := strings.TrimSpace(matches[1])
		if version == "" {
			return "", false
		}
		return version, true
	}
	version := strings.TrimSpace(parsed.VerWebserver)
	if version == "" {
		return "", false
	}
	return version, true
}

func normalizeMACString(raw string) string {
	val := strings.ToLower(strings.TrimSpace(raw))
	val = strings.ReplaceAll(val, "-", ":")
	if val == "" {
		return ""
	}
	parts := strings.Split(val, ":")
	if len(parts) != 6 {
		return ""
	}
	for _, p := range parts {
		if len(p) != 2 {
			return ""
		}
	}
	return val
}
