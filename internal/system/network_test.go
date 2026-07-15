package system

import "testing"

func TestParseConnManWiFiService(t *testing.T) {
	service := parseConnManServiceBlock(`struct {
 string "Type"
 variant string "wifi"
 string "State"
 variant string "online"
 string "Strength"
 variant byte 61
 string "Ethernet"
 variant array [
 string "Interface"
 variant string "wlan0"
 string "Address"
 variant string "00:11:22:33:44:55"
 ]
 string "IPv4"
 variant array [
 string "Address"
 variant string "192.0.2.172"
 string "Netmask"
 variant string "255.255.255.0"
 ]
}`)

	if service.Type != "wifi" || service.State != "online" || service.Interface != "wlan0" || service.IP != "192.0.2.172" || service.Netmask != "255.255.255.0" || service.MAC != "00:11:22:33:44:55" {
		t.Fatalf("unexpected ConnMan service: %#v", service)
	}
	if service.Strength == nil || *service.Strength != 61 {
		t.Fatalf("unexpected WiFi strength: %#v", service.Strength)
	}
}

func TestNormalizeMAC(t *testing.T) {
	if got := normalizeMAC("00-11-22-33-44-55"); got != "00:11:22:33:44:55" {
		t.Fatalf("normalized MAC = %q", got)
	}
}
