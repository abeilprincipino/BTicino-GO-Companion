package openwebnet

import (
	"maps"
	"sync"
	"time"
)

const defaultTraceCapacity = 200

// Trace retains a bounded, WebUI-safe view of received multicast frames.
type Trace struct {
	mu       sync.RWMutex
	capacity int
	frames   []map[string]any
}

func NewTrace(capacity int) *Trace {
	if capacity <= 0 {
		capacity = defaultTraceCapacity
	}

	return &Trace{capacity: capacity}
}

func (t *Trace) Record(message Message, eventCount int) {
	t.record(message.System, message.Raw, eventCount)
}

func (t *Trace) RecordTCP(direction, frame string) {
	t.record("TCP "+direction, frame, 0)
}

func (t *Trace) record(system, raw string, eventCount int) {
	if t == nil {
		return
	}

	frame := map[string]any{
		"t":      time.Now().Format(time.RFC3339Nano),
		"sys":    system,
		"raw":    raw,
		"mapped": eventCount > 0,
		"events": eventCount,
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.frames) == t.capacity {
		copy(t.frames, t.frames[1:])
		t.frames[len(t.frames)-1] = frame

		return
	}

	t.frames = append(t.frames, frame)
}

func (t *Trace) Frames() []map[string]any {
	if t == nil {
		return nil
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	frames := make([]map[string]any, len(t.frames))
	for index, frame := range t.frames {
		frames[index] = make(map[string]any, len(frame))
		maps.Copy(frames[index], frame)
	}

	return frames
}
