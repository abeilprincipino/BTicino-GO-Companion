package media

import (
	"context"
	"errors"
	"testing"
	"time"
)

type backendStub struct {
	startDevAddr string
	startCalls   int
	stopCalls    int
	startErr     error
	stopErr      error
}

type transitionRecorder struct {
	items []Transition
}

func (r *transitionRecorder) record(tr Transition) {
	r.items = append(r.items, tr)
}

func (b *backendStub) StreamStart(_ context.Context, devAddr string) error {
	b.startCalls++
	b.startDevAddr = devAddr
	return b.startErr
}

func (b *backendStub) StreamStop(_ context.Context) error {
	b.stopCalls++
	return b.stopErr
}

func TestServiceManualStartStop(t *testing.T) {
	backend := &backendStub{}
	svc := NewService(backend)

	if err := svc.StartForEntrypoint(context.Background(), "main", "20"); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	snap := svc.Snapshot()
	if !snap.StreamActive || !snap.ManualHold || snap.ActiveEntrypoint != "main" {
		t.Fatalf("unexpected snapshot after start: %+v", snap)
	}
	if err := svc.StopForEntrypoint(context.Background(), "main"); err != nil {
		t.Fatalf("stop failed: %v", err)
	}
	snap = svc.Snapshot()
	if snap.StreamActive || snap.ManualHold {
		t.Fatalf("unexpected snapshot after stop: %+v", snap)
	}
	if backend.startCalls != 1 || backend.stopCalls != 1 {
		t.Fatalf("unexpected backend calls start=%d stop=%d", backend.startCalls, backend.stopCalls)
	}
}

func TestServiceReaderLifecycleAutostartStop(t *testing.T) {
	backend := &backendStub{}
	svc := NewService(backend)
	rec := &transitionRecorder{}
	svc.SetTransitionSink(rec.record)

	if err := svc.ReaderJoin(context.Background(), "s1", "main", "20"); err != nil {
		t.Fatalf("reader join failed: %v", err)
	}
	if err := svc.ReaderLeave(context.Background(), "s1"); err != nil {
		t.Fatalf("reader leave failed: %v", err)
	}
	if backend.startCalls != 1 || backend.stopCalls != 1 {
		t.Fatalf("unexpected backend calls start=%d stop=%d", backend.startCalls, backend.stopCalls)
	}
	if len(rec.items) != 2 {
		t.Fatalf("expected 2 transitions, got %d", len(rec.items))
	}
	if rec.items[0].Kind != "stream.started" || rec.items[0].EntrypointID != "main" || rec.items[0].DevAddr != "20" {
		t.Fatalf("unexpected start transition: %+v", rec.items[0])
	}
	if rec.items[1].Kind != "stream.stopped" || rec.items[1].EntrypointID != "main" || rec.items[1].DevAddr != "20" {
		t.Fatalf("unexpected stop transition: %+v", rec.items[1])
	}
}

func TestServiceEntrySwitchBlockedWithActiveReaders(t *testing.T) {
	backend := &backendStub{}
	svc := NewService(backend)
	if err := svc.ReaderJoin(context.Background(), "s1", "main", "20"); err != nil {
		t.Fatalf("reader join failed: %v", err)
	}
	err := svc.StartForEntrypoint(context.Background(), "gate2", "21")
	if !errors.Is(err, ErrEntrypointSwitchBlocked) {
		t.Fatalf("expected ErrEntrypointSwitchBlocked, got %v", err)
	}
}

func TestServicePruneIdleReadersStopsStream(t *testing.T) {
	backend := &backendStub{}
	svc := NewService(backend)
	if err := svc.ReaderJoin(context.Background(), "s1", "main", "20"); err != nil {
		t.Fatalf("reader join failed: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := svc.PruneIdleReaders(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("prune failed: %v", err)
	}
	if backend.stopCalls != 1 {
		t.Fatalf("expected stopCalls=1 got %d", backend.stopCalls)
	}
}
