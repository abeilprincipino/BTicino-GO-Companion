package ringattribution

import (
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type testSettings struct {
	enabled atomic.Bool
	maxWait atomic.Int64
}

func newTestSettings(enabled bool, maxWait time.Duration) *testSettings {
	s := &testSettings{}
	s.enabled.Store(enabled)
	s.maxWait.Store(int64(maxWait))

	return s
}

func (s *testSettings) get() (bool, time.Duration) {
	return s.enabled.Load(), time.Duration(s.maxWait.Load())
}

func newTestLearner(t *testing.T, dir string, settings *testSettings, doorbells ...string) *Learner {
	t.Helper()

	return New(dir, settings.get, func() []string { return doorbells }, slog.New(slog.DiscardHandler))
}

// The delays are those of the portal6 rings a C100X sent with the address 21
// on 2026-11-20.
func TestLearnerHoldsAnAddressThatMisledForItsSlowestCorrection(t *testing.T) {
	t.Parallel()

	learner := newTestLearner(t, t.TempDir(), newTestSettings(true, time.Second), "portal0", "portal6")

	if hold := learner.Hold("21"); hold != 0 {
		t.Fatalf("Hold(21) before any correction = %v, want 0", hold)
	}

	for _, ms := range []int64{535, 300, 661, 387, 407, 330} {
		learner.Observe("21", "portal6", time.Duration(ms)*time.Millisecond)
	}

	if hold := learner.Hold("21"); hold != 761*time.Millisecond {
		t.Fatalf("Hold(21) = %v, want the slowest correction plus the margin, 761ms", hold)
	}

	if hold := learner.Hold("213"); hold != 0 {
		t.Fatalf("Hold(213) = %v, want 0: that address never misled", hold)
	}
}

func TestLearnerCapsTheHold(t *testing.T) {
	t.Parallel()

	settings := newTestSettings(true, time.Second)
	learner := newTestLearner(t, t.TempDir(), settings, "portal0", "portal6")
	learner.Observe("213", "portal0", 2300*time.Millisecond)

	if hold := learner.Hold("213"); hold != time.Second {
		t.Fatalf("Hold(213) = %v, want the 1s cap", hold)
	}

	settings.maxWait.Store(int64(500 * time.Millisecond))

	if hold := learner.Hold("213"); hold != 500*time.Millisecond {
		t.Fatalf("Hold(213) after lowering the cap = %v, want 500ms", hold)
	}
}

func TestLearnerForgetsAnOutlierAfterTheKeptCorrections(t *testing.T) {
	t.Parallel()

	learner := newTestLearner(t, t.TempDir(), newTestSettings(true, 5*time.Second), "portal6")
	learner.Observe("21", "portal6", 1400*time.Millisecond)

	for range keptObservations {
		learner.Observe("21", "portal6", 400*time.Millisecond)
	}

	if hold := learner.Hold("21"); hold != 500*time.Millisecond {
		t.Fatalf("Hold(21) = %v, want 500ms once the outlier is out of the last %d", hold, keptObservations)
	}
}

func TestLearnerDoesNothingWhileOff(t *testing.T) {
	t.Parallel()

	settings := newTestSettings(false, time.Second)
	learner := newTestLearner(t, t.TempDir(), settings, "portal6")
	learner.Observe("21", "portal6", 400*time.Millisecond)

	settings.enabled.Store(true)

	if hold := learner.Hold("21"); hold != 0 {
		t.Fatalf("Hold(21) = %v, want 0: nothing is learned while off", hold)
	}

	learner.Observe("21", "portal6", 400*time.Millisecond)
	settings.enabled.Store(false)

	if hold := learner.Hold("21"); hold != 0 {
		t.Fatalf("Hold(21) while off = %v, want 0", hold)
	}

	if learned := learner.Learned(); len(learned) != 1 || learned[0].HoldMS != 500 {
		t.Fatalf("Learned() while off = %+v, want what was learned kept", learned)
	}
}

func TestLearnerKeepsWhatItLearnedAcrossARestartAndResets(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	settings := newTestSettings(true, time.Second)
	newTestLearner(t, dir, settings, "portal0", "portal6").Observe("21", "portal6", 600*time.Millisecond)

	restarted := newTestLearner(t, dir, settings, "portal0", "portal6")
	if hold := restarted.Hold("21"); hold != 700*time.Millisecond {
		t.Fatalf("Hold(21) after a restart = %v, want 700ms", hold)
	}

	want := Learned{Address: "21", Corrections: 1, SlowestMS: 600, HoldMS: 700}
	if learned := restarted.Learned(); len(learned) != 1 || learned[0] != want {
		t.Fatalf("Learned() = %+v, want [%+v]", learned, want)
	}

	if err := restarted.Reset(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dir, FileName)); !os.IsNotExist(err) {
		t.Fatalf("learned file after a reset: %v, want it gone", err)
	}

	if hold := newTestLearner(t, dir, settings, "portal0", "portal6").Hold("21"); hold != 0 {
		t.Fatalf("Hold(21) after a reset and a restart = %v, want 0", hold)
	}
}

func TestLearnerIgnoresAnEntrypointThatCanNoLongerRing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	settings := newTestSettings(true, time.Second)
	newTestLearner(t, dir, settings, "portal0", "portal6").Observe("21", "portal6", 600*time.Millisecond)

	if hold := newTestLearner(t, dir, settings, "portal0").Hold("21"); hold != 0 {
		t.Fatalf("Hold(21) with portal6 gone = %v, want 0", hold)
	}
}

func TestLearnerStartsAfreshFromAnUnreadableFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	learner := newTestLearner(t, dir, newTestSettings(true, time.Second), "portal6")
	if hold := learner.Hold("21"); hold != 0 {
		t.Fatalf("Hold(21) = %v, want 0", hold)
	}

	learner.Observe("21", "portal6", 300*time.Millisecond)

	if hold := newTestLearner(t, dir, newTestSettings(true, time.Second), "portal6").Hold("21"); hold != 400*time.Millisecond {
		t.Fatalf("Hold(21) after relearning = %v, want 400ms: the bad file is replaced", hold)
	}
}
