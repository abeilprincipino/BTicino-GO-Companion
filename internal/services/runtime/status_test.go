package runtime

import "testing"

func TestStatusDefaults(t *testing.T) {
	s := New(true, false)
	snap := s.Snapshot()
	if !snap.SIP.Enabled || snap.SIP.Ready {
		t.Fatalf("unexpected sip snapshot: %+v", snap.SIP)
	}
	if !snap.OpenWebNet.Ready {
		t.Fatalf("openwebnet should be ready when disabled: %+v", snap.OpenWebNet)
	}
	if snap.Control.Ready {
		t.Fatalf("control should start not ready: %+v", snap.Control)
	}
}

func TestStatusSetters(t *testing.T) {
	s := New(true, true)
	s.SetSIPReady(true, "")
	s.SetOpenWebNetReady(false, "rx timeout")
	s.SetControlReady(true, "")
	snap := s.Snapshot()
	if !snap.SIP.Ready || snap.SIP.Error != "" {
		t.Fatalf("unexpected sip status: %+v", snap.SIP)
	}
	if snap.OpenWebNet.Ready || snap.OpenWebNet.Error != "rx timeout" {
		t.Fatalf("unexpected openwebnet status: %+v", snap.OpenWebNet)
	}
	if !snap.Control.Ready {
		t.Fatalf("unexpected control status: %+v", snap.Control)
	}
}
