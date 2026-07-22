package homekit

import (
	"bticino-go-companion/internal/config"
	"bytes"
	"strings"
	"testing"
)

func TestSetupURI(t *testing.T) {
	t.Parallel()

	uri, err := SetupURI(config.HomeKit{PIN: "123-45-678", SetupID: "ABCD"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(uri, "X-HM://") || !strings.HasSuffix(uri, "ABCD") {
		t.Fatalf("setup uri = %q", uri)
	}
}

func TestSetupQRCodePNG(t *testing.T) {
	t.Parallel()

	image, err := SetupQRCodePNG(config.HomeKit{PIN: "123-45-678", SetupID: "ABCD"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(image, []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatalf("qr image is not a PNG: %x", image[:min(8, len(image))])
	}
}

func TestSetupURIRejectsInvalidSetupID(t *testing.T) {
	t.Parallel()

	if _, err := SetupURI(config.HomeKit{PIN: "123-45-678", SetupID: "BAD"}); err == nil {
		t.Fatal("SetupURI() succeeded with an invalid setup id")
	}
}
