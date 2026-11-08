package api

import (
	"bticino-go-companion/internal/core"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type audioRecorder struct {
	calls []string
	err   error
}

func (a *audioRecorder) Mute(context.Context) error {
	a.calls = append(a.calls, "mute")
	return a.err
}

func (a *audioRecorder) Unmute(context.Context) error {
	a.calls = append(a.calls, "unmute")
	return a.err
}

// The state a mute command returns and broadcasts must carry the change: the
// bus frame that also reports it can arrive after that broadcast, and Home
// Assistant then keeps the old state (seen 2026-11-05 on a C100X: on at
// 20:20:06.380, back off 5 ms later).
func TestServer_AudioCommandReportsTheConfirmedState(t *testing.T) {
	t.Parallel()

	server, _ := newTestServer(t)
	projector, ok := server.state.(*core.Projector)
	if !ok {
		t.Fatalf("state provider = %T, want *core.Projector", server.state)
	}

	audio := &audioRecorder{}
	server.SetAudio(audio)

	var applied []core.EventType
	server.SetEventSink(func(event core.Event) {
		applied = append(applied, event.Type())
		if _, err := projector.Apply(event); err != nil {
			t.Errorf("apply %s: %v", event.Type(), err)
		}
	})

	token, err := server.auth.RotateBearer()
	if err != nil {
		t.Fatal(err)
	}

	for _, step := range []struct {
		path  string
		muted bool
		event core.EventType
	}{
		{path: "/api/v3/audio/mute", muted: true, event: core.EventAudioMuted},
		{path: "/api/v3/audio/unmute", muted: false, event: core.EventAudioUnmuted},
	} {
		request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, step.path, nil)
		request.Header.Set("Authorization", "Bearer "+token)

		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)

		var body struct {
			State struct {
				Audio core.AudioState `json:"audio"`
			} `json:"state"`
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &body) != nil {
			t.Fatalf("%s response = %d: %s", step.path, response.Code, response.Body.String())
		}

		if body.State.Audio.Muted != step.muted {
			t.Fatalf("%s state muted = %v, want %v", step.path, body.State.Audio.Muted, step.muted)
		}

		if len(applied) == 0 || applied[len(applied)-1] != step.event {
			t.Fatalf("%s applied events = %v, want last %s", step.path, applied, step.event)
		}
	}
}

func TestServer_FailedAudioCommandAppliesNothing(t *testing.T) {
	t.Parallel()

	server, _ := newTestServer(t)
	server.SetAudio(&audioRecorder{err: errors.New("nack")})

	var applied []core.EventType
	server.SetEventSink(func(event core.Event) { applied = append(applied, event.Type()) })

	token, err := server.auth.RotateBearer()
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v3/audio/mute", nil)
	request.Header.Set("Authorization", "Bearer "+token)

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code == http.StatusOK || len(applied) != 0 {
		t.Fatalf("failed mute response = %d, applied = %v", response.Code, applied)
	}
}
