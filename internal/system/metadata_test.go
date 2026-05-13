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

func TestParseConnManServiceBlock(t *testing.T) {
	block := `      struct {
         object path "/net/connman/service/wifi_000350962e38_486f6d656c6963696f7573_managed_psk"
         array [
            dict entry(
               string "Type"
               variant                   string "wifi"
            )
            dict entry(
               string "State"
               variant                   string "online"
            )
            dict entry(
               string "Strength"
               variant                   byte 61
            )
            dict entry(
               string "Ethernet"
               variant                   array [
                     dict entry(
                        string "Interface"
                        variant                            string "wlan0"
                     )
                     dict entry(
                        string "Address"
                        variant                            string "00:03:50:96:2E:38"
                     )
                  ]
            )
            dict entry(
               string "IPv4"
               variant                   array [
                     dict entry(
                        string "Address"
                        variant                            string "10.0.0.172"
                     )
                  ]
            )
         ]
      }`

	svc := parseConnManServiceBlock(block)
	if svc.Type != "wifi" {
		t.Fatalf("unexpected type: %s", svc.Type)
	}
	if svc.State != "online" {
		t.Fatalf("unexpected state: %s", svc.State)
	}
	if svc.Interface != "wlan0" {
		t.Fatalf("unexpected interface: %s", svc.Interface)
	}
	if svc.IP != "10.0.0.172" {
		t.Fatalf("unexpected ip: %s", svc.IP)
	}
	if svc.MAC != "00:03:50:96:2E:38" {
		t.Fatalf("unexpected mac: %s", svc.MAC)
	}
	if svc.Strength == nil || *svc.Strength != 61 {
		t.Fatalf("unexpected strength: %#v", svc.Strength)
	}
}

func TestDetectNetworkSnapshotViaConnManOutput(t *testing.T) {
	output := `method return time=1778655455.803619 sender=:1.0 -> destination=:1.9 serial=173 reply_serial=2
   array [
      struct {
         object path "/net/connman/service/wifi_a_offline"
         array [
            dict entry(
               string "Type"
               variant                   string "wifi"
            )
            dict entry(
               string "State"
               variant                   string "idle"
            )
            dict entry(
               string "Strength"
               variant                   byte 10
            )
            dict entry(
               string "Ethernet"
               variant                   array [
                     dict entry(
                        string "Interface"
                        variant                            string "wlan0"
                     )
                     dict entry(
                        string "Address"
                        variant                            string "00:03:50:96:2E:39"
                     )
                  ]
            )
            dict entry(
               string "IPv4"
               variant                   array [
                     dict entry(
                        string "Address"
                        variant                            string "10.0.0.173"
                     )
                  ]
            )
         ]
      }
      struct {
         object path "/net/connman/service/wifi_b_online"
         array [
            dict entry(
               string "Type"
               variant                   string "wifi"
            )
            dict entry(
               string "State"
               variant                   string "online"
            )
            dict entry(
               string "Strength"
               variant                   byte 61
            )
            dict entry(
               string "Ethernet"
               variant                   array [
                     dict entry(
                        string "Interface"
                        variant                            string "wlan0"
                     )
                     dict entry(
                        string "Address"
                        variant                            string "00:03:50:96:2E:38"
                     )
                  ]
            )
            dict entry(
               string "IPv4"
               variant                   array [
                     dict entry(
                        string "Address"
                        variant                            string "10.0.0.172"
                     )
                  ]
            )
         ]
      }
   ]`

	blocks := splitConnManServiceBlocks(output)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}

	best := connManService{score: -1}
	for _, block := range blocks {
		svc := parseConnManServiceBlock(block)
		svc.score = connManServiceScore(svc.State)
		if svc.score > best.score {
			best = svc
		}
	}

	if best.State != "online" {
		t.Fatalf("expected online service to win, got %s", best.State)
	}
	if best.Strength == nil || *best.Strength != 61 {
		t.Fatalf("unexpected best strength: %#v", best.Strength)
	}
}
