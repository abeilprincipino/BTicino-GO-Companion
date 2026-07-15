package system

import (
	"bticino-go-companion/internal/config"
	"fmt"
	"net"
	"os"
	"strings"
)

func DetectMetadata() (config.Metadata, error) {
	model, err := detectModel()
	if err != nil {
		return config.Metadata{}, err
	}

	mac, err := detectMAC()
	if err != nil {
		return config.Metadata{}, err
	}

	return config.Metadata{Model: model, MAC: mac}, nil
}

func detectModel() (string, error) {
	for _, path := range []string{
		"/etc/opkg/base-feeds.conf",
		"/etc/opkg/arch.conf",
		"/home/bticino/sp/dbfiles_ws.xml",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		upper := strings.ToUpper(string(data))
		switch {
		case strings.Contains(upper, "C300X"):
			return "C300X", nil
		case strings.Contains(upper, "C100X"):
			return "C100X", nil
		}
	}

	return "", fmt.Errorf("detect model: %w", config.ErrMissingMetadata)
}

func detectMAC() (string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", fmt.Errorf("list network interfaces: %w", err)
	}

	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 || len(iface.HardwareAddr) == 0 {
			continue
		}

		return iface.HardwareAddr.String(), nil
	}

	return "", fmt.Errorf("detect mac: %w", config.ErrMissingMetadata)
}
