package openwebnet

import (
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"strings"
	"sync"
	"time"
)

// Mapper keeps enough device context to correlate ring start and stop frames.
type Mapper struct {
	mu               sync.Mutex
	entrypoints      map[string]core.EntrypointID
	recentFrames     map[string]time.Time
	activeEntrypoint core.EntrypointID
	// doorbell is config.DoorbellEntrypoint, if the configuration names one:
	// every ring is its, whatever address the frame carries.
	doorbell core.EntrypointID
}

func NewMapper(entrypoints []config.Entrypoint) *Mapper {
	m := &Mapper{entrypoints: make(map[string]core.EntrypointID), recentFrames: make(map[string]time.Time)}
	for _, entrypoint := range entrypoints {
		m.entrypoints[entrypoint.DevAddr] = core.EntrypointID(entrypoint.ID)
	}

	if doorbell, ok := config.DoorbellEntrypoint(entrypoints); ok {
		m.doorbell = core.EntrypointID(doorbell.ID)
	}

	return m
}

func (m *Mapper) Map(message Message) []core.Event {
	if system := strings.ToLower(strings.TrimSpace(message.System)); system != "open" && system != "aswm" {
		return nil
	}

	raw := strings.TrimSpace(message.Raw)
	if raw == "" {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	if m.duplicate(raw, now) {
		return nil
	}

	if IsRingStart(raw) {
		if m.activeEntrypoint != "" {
			return nil
		}

		id := m.resolveEntrypoint(raw)
		if id == "" {
			return nil
		}

		m.activeEntrypoint = id

		return []core.Event{core.RingStarted{EntrypointID: id}}
	}

	if raw == FrameAudioMuted {
		return []core.Event{core.AudioMuted{}}
	}

	if raw == FrameAudioUnmuted {
		return []core.Event{core.AudioUnmuted{}}
	}

	if enabled, _, ok := ParseVoicemailStatus(raw); ok {
		if enabled {
			return []core.Event{core.VoicemailEnabled{}}
		}

		return []core.Event{core.VoicemailDisabled{}}
	}

	if IsStreamStop(raw) || IsFreeAVResources(raw) {
		if m.activeEntrypoint == "" {
			return nil
		}

		events := []core.Event{core.RingCleared{EntrypointID: m.activeEntrypoint}}
		m.activeEntrypoint = ""

		return events
	}

	return nil
}

func (m *Mapper) duplicate(raw string, now time.Time) bool {
	for frame, seen := range m.recentFrames {
		if now.Sub(seen) > 300*time.Millisecond {
			delete(m.recentFrames, frame)
		}
	}

	if seen, ok := m.recentFrames[raw]; ok && now.Sub(seen) <= 300*time.Millisecond {
		m.recentFrames[raw] = now
		return true
	}

	m.recentFrames[raw] = now

	return false
}

// resolveEntrypoint gives a ring frame to the doorbell entrypoint when the
// configuration names one: an entrypoint that is only a lock or a light may
// share its devaddr with an address in the frame. Otherwise the frame
// *8*1#<mode>#4#<a>*<b>## is matched to a configured devaddr, trying <b>, then
// <a>, which is where a view request carries the external unit.
func (m *Mapper) resolveEntrypoint(frame string) core.EntrypointID {
	if m.doorbell != "" {
		return m.doorbell
	}

	if id := m.entrypoints[ExtractAddress(frame)]; id != "" {
		return id
	}

	if id := m.entrypoints[extractViewRequestAddress(frame)]; id != "" {
		return id
	}

	if len(m.entrypoints) == 1 {
		for _, id := range m.entrypoints {
			return id
		}
	}

	return ""
}
