package system

import (
	"bufio"
	"encoding/xml"
	"net"
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
	ifaces, err := net.Interfaces()
	if err != nil {
		return NetworkSnapshot{}
	}

	type candidate struct {
		ifaceName string
		ip        string
		mac       string
		score     int
	}
	best := candidate{score: -1}
	for _, iface := range ifaces {
		if (iface.Flags&net.FlagLoopback) != 0 || (iface.Flags&net.FlagUp) == 0 {
			continue
		}
		ip := interfacePrimaryIP(iface)
		if ip == "" {
			continue
		}
		score := interfacePriority(iface.Name)
		if score > best.score {
			best = candidate{
				ifaceName: iface.Name,
				ip:        ip,
				mac:       strings.ToLower(strings.TrimSpace(iface.HardwareAddr.String())),
				score:     score,
			}
		}
	}
	if best.score < 0 {
		return NetworkSnapshot{}
	}

	snap := NetworkSnapshot{
		Interface: best.ifaceName,
		IP:        best.ip,
		MAC:       best.mac,
	}
	if rssi, ok := readWirelessRSSI(best.ifaceName); ok {
		snap.WiFiRSSI = &rssi
	}
	return snap
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

func interfacePrimaryIP(iface net.Interface) string {
	addrs, err := iface.Addrs()
	if err != nil {
		return ""
	}

	var firstGlobalV6 string
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP == nil {
			continue
		}
		ip := ipNet.IP
		if ip.IsLoopback() || !ip.IsGlobalUnicast() {
			continue
		}
		if ipv4 := ip.To4(); ipv4 != nil {
			return ipv4.String()
		}
		if firstGlobalV6 == "" {
			firstGlobalV6 = ip.String()
		}
	}
	return firstGlobalV6
}

func interfacePriority(name string) int {
	lower := strings.ToLower(strings.TrimSpace(name))
	switch {
	case strings.HasPrefix(lower, "wlan"), strings.HasPrefix(lower, "wl"):
		return 40
	case strings.HasPrefix(lower, "usb"):
		return 20
	case strings.HasPrefix(lower, "eth"), strings.HasPrefix(lower, "en"):
		return 10
	default:
		return 5
	}
}

func readWirelessRSSI(ifaceName string) (int, bool) {
	if strings.TrimSpace(ifaceName) == "" {
		return 0, false
	}
	out, err := exec.Command("/usr/sbin/iw", "dev", ifaceName, "link").Output()
	if err != nil {
		return 0, false
	}
	return parseIWLinkRSSI(string(out))
}

func parseIWLinkRSSI(output string) (int, bool) {
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(strings.ToLower(line), "signal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, false
		}
		value, err := strconv.Atoi(fields[1])
		if err != nil {
			return 0, false
		}
		return value, true
	}
	return 0, false
}
