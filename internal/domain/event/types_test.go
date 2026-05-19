package event

import "testing"

func TestIsKnownType(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{name: "ring started", in: TypeRingStarted, want: true},
		{name: "stream stopped", in: TypeStreamStopped, want: true},
		{name: "audio muted", in: TypeAudioMuted, want: true},
		{name: "unknown type", in: "something.else", want: false},
		{name: "empty", in: "", want: false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := IsKnownType(tc.in)
			if got != tc.want {
				t.Fatalf("IsKnownType(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

