package openwebnet

import (
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"strings"
	"sync"
	"time"
)

const (
	// stopBurstBeforeRing is how close to a stop frame a ring start has to come
	// to be a ring restarted by a second press of the doorbell. The intercom ends
	// the first ring with a burst of stop frames, still arriving after the new
	// ring start: about 0.5 s of them on a C100X.
	stopBurstBeforeRing = time.Second
	// staleStopWindow is how long after such a ring start a stop frame is taken
	// for the tail of that burst rather than for the end of the new ring.
	staleStopWindow = 1500 * time.Millisecond
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
	// ringers are the entrypoints that can ring, by devaddr, and ringerOrder
	// the same in configuration order. With more than one doorbell, one per
	// external unit, a ring goes to the one at the frame's address.
	ringers     map[string]core.EntrypointID
	ringerOrder []core.EntrypointID
	// lastStop is the latest stop frame, duplicates included.
	lastStop time.Time
	// ignoreStopsUntil is the end of the stale stop window of a restarted ring.
	ignoreStopsUntil time.Time
	now              func() time.Time
}

func NewMapper(entrypoints []config.Entrypoint) *Mapper {
	m := &Mapper{entrypoints: make(map[string]core.EntrypointID), ringers: make(map[string]core.EntrypointID), recentFrames: make(map[string]time.Time), now: time.Now}
	for _, entrypoint := range entrypoints {
		m.entrypoints[entrypoint.DevAddr] = core.EntrypointID(entrypoint.ID)

		if entrypoint.Capabilities.Ring {
			m.ringers[strings.TrimSpace(entrypoint.DevAddr)] = core.EntrypointID(entrypoint.ID)
			m.ringerOrder = append(m.ringerOrder, core.EntrypointID(entrypoint.ID))
		}
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

	now := m.now()
	isStop := IsStreamStop(raw) || IsFreeAVResources(raw)

	// Before the duplicate check: the burst is mostly duplicates, and its last
	// frame is what dates it.
	if isStop {
		m.lastStop = now
	}

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

		if !m.lastStop.IsZero() && now.Sub(m.lastStop) <= stopBurstBeforeRing {
			m.ignoreStopsUntil = now.Add(staleStopWindow)
		}

		return []core.Event{core.RingStarted{EntrypointID: id}}
	}

	if address, ok := ParseRingIdentityAddress(raw); ok {
		return m.retarget(address)
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

	if isStop {
		if m.activeEntrypoint == "" || now.Before(m.ignoreStopsUntil) {
			return nil
		}

		events := []core.Event{core.RingCleared{EntrypointID: m.activeEntrypoint}}
		m.activeEntrypoint = ""

		return events
	}

	return nil
}

// retarget moves the ring in progress to the doorbell at address, which a
// ring identity frame names as the external unit whose camera rings. A C100X
// sends every ring frame with the address of one unit, whichever unit rang,
// and names the right one only in this later frame. It only applies with
// several doorbells: a configuration that names one doorbell gets every ring.
func (m *Mapper) retarget(address string) []core.Event {
	if m.activeEntrypoint == "" || m.doorbell != "" {
		return nil
	}

	id := m.ringers[address]
	if id == "" || id == m.activeEntrypoint {
		return nil
	}

	from := m.activeEntrypoint
	m.activeEntrypoint = id

	return []core.Event{core.RingRetargeted{From: from, To: id}}
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
// share its devaddr with an address in the frame. With several doorbells, one
// per external unit, the frame goes to the doorbell at its address, and to the
// first doorbell in configuration order when the address matches none, so a
// ring is never dropped. Otherwise the frame *8*1#<mode>#4#<a>*<b>## is matched
// to a configured devaddr, trying <b>, then <a>, which is where a ring or a
// view request carries the external unit.
func (m *Mapper) resolveEntrypoint(frame string) core.EntrypointID {
	if m.doorbell != "" {
		return m.doorbell
	}

	if len(m.ringerOrder) > 0 {
		for _, address := range []string{ExtractAddress(frame), extractViewRequestAddress(frame)} {
			if id := m.ringers[address]; id != "" {
				return id
			}
		}

		return m.ringerOrder[0]
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
