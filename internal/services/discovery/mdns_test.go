package discovery

import "testing"

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
