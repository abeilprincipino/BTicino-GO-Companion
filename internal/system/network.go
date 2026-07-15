package system

import (
	"bufio"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
)

func PreferredOutboundInterface() (net.Interface, error) {
	conn, err := net.Dial("udp4", "8.8.8.8:80")
	if err != nil {
		return net.Interface{}, fmt.Errorf("detect outbound ipv4: %w", err)
	}
	defer conn.Close() //nolint:errcheck // UDP probe is complete

	address, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || address.IP.To4() == nil {
		return net.Interface{}, fmt.Errorf("detect outbound ipv4")
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return net.Interface{}, fmt.Errorf("list interfaces: %w", err)
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagMulticast == 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, ifaceAddress := range addresses {
			if network, ok := ifaceAddress.(*net.IPNet); ok && network.Contains(address.IP) {
				return iface, nil
			}
		}
	}

	return net.Interface{}, fmt.Errorf("map outbound ipv4 to interface")
}

// NetworkSnapshot describes the connected WiFi service reported by ConnMan.
type NetworkSnapshot struct {
	Interface    string
	IP           string
	Netmask      string
	MAC          string
	WiFiStrength *int
}

// DetectNetworkSnapshot reads the active WiFi service from ConnMan. This uses
// the same source as the V2 companion rather than kernel interface ordering.
func DetectNetworkSnapshot() (NetworkSnapshot, bool) {
	out, err := exec.Command("/usr/bin/dbus-send", "--system", "--print-reply", "--dest=net.connman", "/", "net.connman.Manager.GetServices").Output()
	if err != nil {
		return NetworkSnapshot{}, false
	}

	best := connManService{score: -1}
	for _, block := range splitConnManServiceBlocks(string(out)) {
		service := parseConnManServiceBlock(block)
		if !strings.EqualFold(strings.TrimSpace(service.Type), "wifi") || strings.TrimSpace(service.Interface) == "" || strings.TrimSpace(service.IP) == "" {
			continue
		}
		service.score = connManServiceScore(service.State)
		if service.score > best.score {
			best = service
		}
	}
	if best.score < 0 {
		return NetworkSnapshot{}, false
	}

	snapshot := NetworkSnapshot{Interface: strings.TrimSpace(best.Interface), IP: strings.TrimSpace(best.IP), Netmask: strings.TrimSpace(best.Netmask), MAC: normalizeMAC(best.MAC)}
	if best.Strength != nil {
		strength := max(0, min(100, *best.Strength))
		snapshot.WiFiStrength = &strength
	}
	return snapshot, true
}

type connManService struct {
	Type, State, Interface, IP, Netmask, MAC string
	Strength                                 *int
	score                                    int
}

func splitConnManServiceBlocks(output string) []string {
	var blocks []string
	depth := 0
	var current strings.Builder
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimSpace(raw)
		if strings.Contains(line, "struct {") && depth == 0 {
			depth = 1
			current.Reset()
			current.WriteString(raw + "\n")
			continue
		}
		if depth == 0 {
			continue
		}
		open := strings.Count(line, "{")
		if open == 1 && strings.Contains(line, "struct {") {
			open = 0
		}
		depth += open - strings.Count(line, "}")
		current.WriteString(raw + "\n")
		if depth <= 0 {
			blocks = append(blocks, current.String())
			depth = 0
		}
	}
	return blocks
}

func parseConnManServiceBlock(block string) connManService {
	var service connManService
	section, key := "top", ""
	scanner := bufio.NewScanner(strings.NewReader(block))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if parsed, ok := parseDBusKey(line); ok {
			key = parsed
			switch key {
			case "Ethernet":
				section, key = "ethernet", ""
			case "IPv4":
				section, key = "ipv4", ""
			case "IPv6", "Proxy", "Provider":
				section, key = "other", ""
			}
			continue
		}
		if !strings.Contains(line, "variant") || key == "" {
			continue
		}
		value, ok := parseDBusVariantValue(line)
		if !ok {
			continue
		}
		switch section {
		case "top":
			switch key {
			case "Type":
				service.Type = value
			case "State":
				service.State = value
			case "Strength":
				if strength, err := strconv.Atoi(value); err == nil {
					service.Strength = &strength
				}
			}
		case "ethernet":
			if key == "Interface" {
				service.Interface = value
			} else if key == "Address" {
				service.MAC = value
			}
		case "ipv4":
			if key == "Address" {
				service.IP = value
			} else if key == "Netmask" {
				service.Netmask = value
			}
		}
	}
	return service
}

func parseDBusKey(line string) (string, bool) {
	if !strings.HasPrefix(line, "string \"") {
		return "", false
	}
	rest := line[len("string \""):]
	end := strings.Index(rest, "\"")
	if end < 0 {
		return "", false
	}
	key := strings.TrimSpace(rest[:end])
	return key, key != ""
}

func parseDBusVariantValue(line string) (string, bool) {
	parts := strings.Fields(line)
	for index, field := range parts {
		if field != "variant" || index+1 >= len(parts) {
			continue
		}
		switch parts[index+1] {
		case "string":
			start := strings.Index(line, "\"")
			if start < 0 {
				return "", false
			}
			rest := line[start+1:]
			end := strings.LastIndex(rest, "\"")
			if end < 0 {
				return "", false
			}
			return strings.TrimSpace(rest[:end]), true
		case "byte", "int16", "int32", "uint16", "uint32", "int64", "uint64":
			if index+2 < len(parts) {
				return strings.TrimSpace(parts[index+2]), true
			}
		}
		return "", false
	}
	return "", false
}

func connManServiceScore(state string) int {
	switch strings.ToLower(strings.TrimSpace(state)) {
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

func normalizeMAC(raw string) string {
	mac := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(raw)), "-", ":")
	parts := strings.Split(mac, ":")
	if len(parts) != 6 {
		return ""
	}
	for _, part := range parts {
		if len(part) != 2 {
			return ""
		}
	}
	return mac
}
