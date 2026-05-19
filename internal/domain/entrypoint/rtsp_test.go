package entrypoint

import "testing"

func TestRTSPPathByEntrypointID(t *testing.T) {
	paths := RTSPPathByEntrypointID([]Model{
		{ID: "garage", DevAddr: "20", HasStream: true},
		{ID: "Gate 1", DevAddr: "21", HasStream: true},
		{ID: "muted", DevAddr: "22", HasStream: false},
	})

	if paths["garage"] != "doorbell-garage" {
		t.Fatalf("unexpected garage path: %q", paths["garage"])
	}
	if paths["Gate 1"] != "doorbell-gate-1" {
		t.Fatalf("unexpected gate path: %q", paths["Gate 1"])
	}
	if _, ok := paths["muted"]; ok {
		t.Fatalf("non-stream entrypoint should not get rtsp path: %+v", paths)
	}
}

func TestRTSPRoutesDisambiguatesSanitizedIDs(t *testing.T) {
	routes := RTSPRoutes([]Model{
		{ID: "gate 1", DevAddr: "20", HasStream: true},
		{ID: "gate-1", DevAddr: "21", HasStream: true},
	})

	if _, ok := routes["doorbell-gate-1"]; !ok {
		t.Fatalf("expected first route, got %+v", routes)
	}
	if _, ok := routes["doorbell-gate-1-2"]; !ok {
		t.Fatalf("expected disambiguated route, got %+v", routes)
	}
}
