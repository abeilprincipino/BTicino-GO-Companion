package discovery

import (
	"context"
	"net"
	"testing"

	"bticino-go-companion/internal/config"
)

func TestNormalizeServiceType(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "default", in: "", want: "_bticomp._tcp"},
		{name: "already_tcp", in: "_bticomp._tcp", want: "_bticomp._tcp"},
		{name: "bare", in: "bticomp", want: "_bticomp._tcp"},
		{name: "udp", in: "_bticomp._udp", want: "_bticomp._udp"},
		{name: "trailing_dot", in: "_bticomp._tcp.", want: "_bticomp._tcp"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeServiceType(tc.in)
			if got != tc.want {
				t.Fatalf("normalizeServiceType(%q)=%q want=%q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParsePort(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{name: "host_port", in: "0.0.0.0:8080", want: 8080},
		{name: "colon_port", in: ":8090", want: 8090},
		{name: "port_only", in: "9000", want: 9000},
		{name: "invalid", in: "not-a-port", want: 8080},
		{name: "empty", in: "", want: 8080},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parsePort(tc.in)
			if got != tc.want {
				t.Fatalf("parsePort(%q)=%d want=%d", tc.in, got, tc.want)
			}
		})
	}
}

func TestSnapshotPrefersDeviceIDForInstance(t *testing.T) {
	state := snapshot("BTicino_Companion", func() bool { return true }, func() string { return "C300X-abc" })
	if state.instanceName != "C300X-abc" {
		t.Fatalf("unexpected instance name: %s", state.instanceName)
	}
	if !state.needsClaim {
		t.Fatal("expected needsClaim to be true")
	}
}

func TestTXTRecordsIncludeHomeAssistantDiscoveryHints(t *testing.T) {
	state := advertisementState{
		deviceName: "BTicino Companion",
		deviceID:   "c300x_123",
		needsClaim: true,
	}
	records := txtRecords(testConfig(), state)

	want := map[string]bool{
		"api=v2":                 false,
		"scheme=http":            false,
		"name=BTicino_Companion": false,
		"device_id=c300x_123":    false,
		"needs_claim=true":       false,
	}
	for _, record := range records {
		if _, ok := want[record]; ok {
			want[record] = true
		}
	}
	for record, found := range want {
		if !found {
			t.Fatalf("missing TXT record %q in %v", record, records)
		}
	}
}

func TestAddrContainsIP(t *testing.T) {
	_, network, err := net.ParseCIDR("10.0.0.172/24")
	if err != nil {
		t.Fatal(err)
	}
	if !addrContainsIP(network, net.ParseIP("10.0.0.172")) {
		t.Fatal("expected network to contain IP")
	}
	if addrContainsIP(network, net.ParseIP("192.168.129.1")) {
		t.Fatal("did not expect network to contain USB-side IP")
	}
}

func testConfig() config.Config {
	cfg := config.Default()
	cfg.DeviceModel = "C300X"
	cfg.DeviceFirmware = "1.7.19"
	return cfg
}

func TestParseValidPort(t *testing.T) {
	if got, ok := parseValidPort("8080"); !ok || got != 8080 {
		t.Fatalf("expected valid 8080, got=%d ok=%v", got, ok)
	}
	if _, ok := parseValidPort("0"); ok {
		t.Fatal("expected invalid 0 port")
	}
	if _, ok := parseValidPort("70000"); ok {
		t.Fatal("expected invalid out-of-range port")
	}
	if _, ok := parseValidPort("abc"); ok {
		t.Fatal("expected invalid non-numeric port")
	}
}

func TestNormalizeNameAndTXT(t *testing.T) {
	if got := normalizeTXT(" BTicino Companion "); got != "BTicino_Companion" {
		t.Fatalf("unexpected normalizeTXT output: %q", got)
	}
	if got := normalizeTXT(" "); got != "" {
		t.Fatalf("expected empty normalizeTXT for blank input, got %q", got)
	}
	if got := normalizeName(" BTicino Companion "); got != "BTicino_Companion" {
		t.Fatalf("unexpected normalizeName output: %q", got)
	}
	if got := normalizeName(" "); got != "" {
		t.Fatalf("expected empty normalizeName for blank input, got %q", got)
	}
}

func TestInterfaceHelpersDefensivePaths(t *testing.T) {
	iface, ok := interfaceForIP(nil)
	if ok || iface.Name != "" {
		t.Fatalf("expected no interface for nil ip, got iface=%+v ok=%v", iface, ok)
	}

	// Ensure helper paths are safe to call in tests across environments.
	_ = outboundIPv4()
	_ = advertisementInterfaces()
}

func TestStartDisabledReturnsImmediately(t *testing.T) {
	cfg := config.Default()
	cfg.MDNSEnabled = false
	if err := Start(context.Background(), cfg, nil, nil, nil); err != nil {
		t.Fatalf("expected disabled mdns start to return nil, got %v", err)
	}
}

func TestSnapshotWithNilCallbacks(t *testing.T) {
	state := snapshot("BTicino_Companion", nil, nil)
	if state.instanceName != "BTicino_Companion" {
		t.Fatalf("unexpected instance name: %s", state.instanceName)
	}
	if state.deviceID != "" || state.needsClaim {
		t.Fatalf("unexpected state with nil callbacks: %+v", state)
	}
}
