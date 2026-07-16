package media

import (
	"errors"
	"strings"
)

var ErrUnsupportedModel = errors.New("media: unsupported intercom model")

// Profile contains only device behavior proven for a supported model.
// Unknown models must never inherit C300X behavior.
type Profile struct {
	Model        string
	HighResVideo bool
}

func ResolveProfile(model string) (Profile, error) {
	switch strings.TrimSpace(strings.ToUpper(model)) {
	case "C300X":
		return Profile{Model: "C300X", HighResVideo: true}, nil
	case "C100X":
		return Profile{Model: "C100X", HighResVideo: false}, nil
	default:
		return Profile{}, ErrUnsupportedModel
	}
}
