package openwebnet

import (
	"context"
	"testing"
	"time"
)

const testVideoParameters = "*7*77#800#480#2500#148#83#0#800#800#12#15#640#480#0#4000*##"

func TestAVReadinessReleasesTheWaitOnTheVideoParameters(t *testing.T) {
	readiness := NewAVReadiness()
	readiness.Observe("*8*1#1#4#21*11##")

	go func() {
		time.Sleep(20 * time.Millisecond)
		readiness.Observe(testVideoParameters)
	}()

	started := time.Now()
	if !readiness.WaitVideoParameters(context.Background(), 2*time.Second) {
		t.Fatal("wait reported no video parameters")
	}

	if waited := time.Since(started); waited > time.Second {
		t.Fatalf("wait took %v, want it released by the frame", waited)
	}
}

func TestAVReadinessGivesUpAfterTheLimit(t *testing.T) {
	readiness := NewAVReadiness()

	if readiness.WaitVideoParameters(context.Background(), 30*time.Millisecond) {
		t.Fatal("wait reported video parameters that never came")
	}
}

func TestAVReadinessForgetsTheParametersOfThePreviousRing(t *testing.T) {
	readiness := NewAVReadiness()
	readiness.Observe(testVideoParameters)

	if !readiness.WaitVideoParameters(context.Background(), time.Millisecond) {
		t.Fatal("parameters seen before the wait are not ready")
	}

	readiness.Observe("*8*1#1#4#21*11##")

	if readiness.WaitVideoParameters(context.Background(), 30*time.Millisecond) {
		t.Fatal("a new ring still had the previous ring's parameters")
	}
}
