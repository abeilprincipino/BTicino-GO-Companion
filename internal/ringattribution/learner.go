// Package ringattribution learns how long a ring has to be held before it is
// reported, on an installation with several doorbells whose intercom may send
// the ring frame with one external unit's address and name the unit that rang
// only in a later identity frame.
//
// It learns per ring-frame address, from the rings whose identity frame named
// another doorbell than the one at that address: how late that frame came. An
// address that never misled gets no hold at all, so an installation whose ring
// frames are right pays nothing. What it learned is kept in a file next to the
// configuration, so it survives a restart.
package ringattribution

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"bticino-go-companion/internal/storage"
)

const (
	// FileName is the file below the data directory that keeps what was learned.
	FileName = "ring_attribution.json"
	// keptObservations is how many corrections are kept per address: the hold
	// follows the slowest of them, so an outlier ages out.
	keptObservations = 20
	// margin is added to the slowest correction.
	margin      = 100 * time.Millisecond
	fileVersion = 1
)

// Settings reports whether learning is on and the cap of a hold.
type Settings func() (enabled bool, maxWait time.Duration)

// Doorbells reports the entrypoints that can ring now. A correction towards an
// entrypoint no longer among them is ignored.
type Doorbells func() []string

// Learned is what was learned for one ring-frame address.
type Learned struct {
	Address string `json:"address"`
	// Corrections is how many rings with this address turned out to be another
	// doorbell's.
	Corrections int `json:"corrections"`
	// SlowestMS is how late the slowest of them was named.
	SlowestMS int64 `json:"slowest_ms"`
	// HoldMS is how long a ring with this address is held while learning is on.
	HoldMS int64 `json:"hold_ms"`
}

type correction struct {
	EntrypointID string `json:"entrypoint_id"`
	DelayMS      int64  `json:"delay_ms"`
}

type document struct {
	Version   int                     `json:"version"`
	Addresses map[string][]correction `json:"addresses"`
}

// Learner is safe for concurrent use.
type Learner struct {
	path      string
	settings  Settings
	doorbells Doorbells
	logger    *slog.Logger

	// saveMu runs the file writes and removals one at a time, each with the
	// state current when it runs, so the file ends with the newest state.
	saveMu sync.Mutex

	mu          sync.Mutex
	corrections map[string][]correction
}

// New reads what was learned from dir, if anything. A file it cannot read is
// logged and learning starts afresh.
func New(dir string, settings Settings, doorbells Doorbells, logger *slog.Logger) *Learner {
	if logger == nil {
		logger = slog.Default()
	}

	l := &Learner{
		path: filepath.Join(dir, FileName), settings: settings, doorbells: doorbells,
		logger: logger.With("component", "ring_attribution"), corrections: make(map[string][]correction),
	}

	if err := l.load(); err != nil {
		l.logger.Warn("ring attribution learning starts afresh; the learned file is unreadable", "file", l.path, "error", err)
	}

	return l
}

// Hold is how long a ring whose frame carries address is held before it is
// reported: the slowest correction learned for address plus a margin, up to
// the cap. It is zero while learning is off or for an address that never
// misled.
func (l *Learner) Hold(address string) time.Duration {
	enabled, maxWait := l.settings()
	if !enabled {
		return 0
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	return l.holdLocked(address, maxWait)
}

// Observe records that a ring whose frame carried address was in fact
// entrypointID's, named delay after the ring frame. It does nothing while
// learning is off.
func (l *Learner) Observe(address, entrypointID string, delay time.Duration) {
	if enabled, _ := l.settings(); !enabled || address == "" || entrypointID == "" || delay < 0 {
		return
	}

	l.mu.Lock()

	kept := append(l.current(address), correction{EntrypointID: entrypointID, DelayMS: delay.Milliseconds()})
	if len(kept) > keptObservations {
		kept = kept[len(kept)-keptObservations:]
	}

	l.corrections[address] = kept
	l.mu.Unlock()

	l.logger.Info("ring attribution learned a correction", "address", address, "entrypoint_id", entrypointID,
		"delay_ms", delay.Milliseconds(), "corrections", len(kept))

	if err := l.save(); err != nil {
		l.logger.Warn("save ring attribution learning", "file", l.path, "error", err)
	}
}

// Reset forgets everything learned.
func (l *Learner) Reset() error {
	l.saveMu.Lock()
	defer l.saveMu.Unlock()

	l.mu.Lock()
	l.corrections = make(map[string][]correction)
	l.mu.Unlock()

	if err := os.Remove(l.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove ring attribution learning: %w", err)
	}

	l.logger.Info("ring attribution learning reset")

	return nil
}

// Learned lists what was learned, by address.
func (l *Learner) Learned() []Learned {
	_, maxWait := l.settings()

	l.mu.Lock()
	defer l.mu.Unlock()

	learned := make([]Learned, 0, len(l.corrections))

	for address := range l.corrections {
		kept := l.current(address)
		if len(kept) == 0 {
			continue
		}

		learned = append(learned, Learned{
			Address: address, Corrections: len(kept), SlowestMS: slowest(kept).Milliseconds(),
			HoldMS: l.holdLocked(address, maxWait).Milliseconds(),
		})
	}

	sort.Slice(learned, func(i, j int) bool { return learned[i].Address < learned[j].Address })

	return learned
}

func (l *Learner) holdLocked(address string, maxWait time.Duration) time.Duration {
	kept := l.current(address)
	if len(kept) == 0 {
		return 0
	}

	return min(slowest(kept)+margin, maxWait)
}

// current is what is kept for address, without the corrections towards an
// entrypoint that can no longer ring.
func (l *Learner) current(address string) []correction {
	doorbells := l.doorbells()

	var kept []correction

	for _, c := range l.corrections[address] {
		if slices.Contains(doorbells, c.EntrypointID) {
			kept = append(kept, c)
		}
	}

	return kept
}

func slowest(kept []correction) time.Duration {
	var delay int64
	for _, c := range kept {
		delay = max(delay, c.DelayMS)
	}

	return time.Duration(delay) * time.Millisecond
}

func (l *Learner) documentLocked() document {
	addresses := make(map[string][]correction, len(l.corrections))
	for address := range l.corrections {
		if kept := l.current(address); len(kept) > 0 {
			addresses[address] = kept
		}
	}

	return document{Version: fileVersion, Addresses: addresses}
}

func (l *Learner) load() error {
	data, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return err
	}

	var doc document
	if err := json.Unmarshal(data, &doc); err != nil {
		return err
	}

	if doc.Version != fileVersion {
		return fmt.Errorf("unknown version %d", doc.Version)
	}

	for address, kept := range doc.Addresses {
		if len(kept) > keptObservations {
			kept = kept[len(kept)-keptObservations:]
		}

		l.corrections[address] = kept
	}

	return nil
}

func (l *Learner) save() error {
	l.saveMu.Lock()
	defer l.saveMu.Unlock()

	l.mu.Lock()
	doc := l.documentLocked()
	l.mu.Unlock()

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}

	return storage.WritePrivateFile(l.path, ".ring-attribution-*.json", append(data, '\n'), false)
}
