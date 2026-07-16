package media

import (
	"errors"
	"testing"
)

func TestResolveProfile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		model   string
		profile Profile
		wantErr bool
	}{
		{name: "c300x", model: "C300X", profile: Profile{Model: "C300X", HighResVideo: true}},
		{name: "c100x", model: "C100X", profile: Profile{Model: "C100X", HighResVideo: false}},
		{name: "unsupported", model: "unknown", wantErr: true},
		{name: "empty", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			profile, err := ResolveProfile(test.model)
			if test.wantErr {
				if !errors.Is(err, ErrUnsupportedModel) {
					t.Fatalf("ResolveProfile(%q) error = %v", test.model, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if profile != test.profile {
				t.Fatalf("ResolveProfile(%q) = %#v, want %#v", test.model, profile, test.profile)
			}
		})
	}
}
