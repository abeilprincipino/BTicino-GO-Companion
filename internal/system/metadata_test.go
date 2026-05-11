package system

import "testing"

func TestParseModel(t *testing.T) {
	if got, ok := parseModel("src/gz uri-zia-0 http://local-server-opkg/C300X/zia"); !ok || got != "C300X" {
		t.Fatalf("expected C300X, got %q ok=%v", got, ok)
	}
	if got, ok := parseModel("src/gz uri-shark-0 http://local-server-opkg/C100X/shark"); !ok || got != "C100X" {
		t.Fatalf("expected C100X, got %q ok=%v", got, ok)
	}
	if got, ok := parseModel("no model"); ok || got != "" {
		t.Fatalf("expected unknown, got %q ok=%v", got, ok)
	}
}

func TestParseFirmwareVersionFromGeneralInfo(t *testing.T) {
	content := `<?xml version="1.0"?>
<Root_Element>
	<general_info>
		<ver_webserver>1.7.19</ver_webserver>
	</general_info>
</Root_Element>`

	got, ok := parseFirmwareVersion(content)
	if !ok || got != "1.7.19" {
		t.Fatalf("expected firmware 1.7.19, got %q ok=%v", got, ok)
	}
}

func TestParseFirmwareVersionWithISO88591Declaration(t *testing.T) {
	content := `<?xml version="1.0" encoding="ISO-8859-1" ?>
<Root_Element>
	<general_info>
		<ver_webserver>1.7.19</ver_webserver>
	</general_info>
</Root_Element>`

	got, ok := parseFirmwareVersion(content)
	if !ok || got != "1.7.19" {
		t.Fatalf("expected firmware 1.7.19 with ISO-8859-1 declaration, got %q ok=%v", got, ok)
	}
}

func TestParseFirmwareVersionRejectsMalformedXML(t *testing.T) {
	content := `<broken><xml><ver_webserver>1.7.19</ver_webserver>`
	got, ok := parseFirmwareVersion(content)
	if ok || got != "" {
		t.Fatalf("expected empty firmware for malformed xml, got %q ok=%v", got, ok)
	}
}
