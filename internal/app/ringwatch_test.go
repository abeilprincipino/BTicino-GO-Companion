package app

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"bticino-go-companion/internal/media"

	"github.com/pion/rtp"
)

const (
	testRingFrame           = "*8*1#1#4#21*11##"
	testActivationFrame     = "*7*58#8#0#0#1*##"
	testIntercomStreamFrame = "*7*300#127#0#0#1#5002#1*##"
	testOwnStreamFrame      = "*7*300#127#0#0#1#46168#1*##"
)

type fakeRingAV struct {
	media.SourceAV
	videoRequests chan time.Time
}

func (a *fakeRingAV) StartVideo(context.Context, bool, int, media.FlowProbe) error {
	a.videoRequests <- time.Now()
	return nil
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func newTestRingWatch(fallback time.Duration) (*ringVideoWatch, *syncBuffer) {
	logs := &syncBuffer{}
	watch := newRingVideoWatch(slog.New(slog.NewTextHandler(logs, nil)))
	watch.fallback = fallback

	return watch, logs
}

func startVideoAsync(av media.SourceAV) chan error {
	done := make(chan error, 1)

	go func() { done <- av.StartVideo(context.Background(), false, 40000, nil) }()

	return done
}

func TestReactiveRingVideoWaitsForTheIntercomStream(t *testing.T) {
	watch, logs := newTestRingWatch(5 * time.Second)
	fake := &fakeRingAV{videoRequests: make(chan time.Time, 2)}

	watch.observeFrame(testRingFrame)
	watch.observeFrame(testActivationFrame)

	done := startVideoAsync(watch.sourceAV(fake))

	// Our own request echoed back is not the intercom's stream.
	watch.observeFrame(testOwnStreamFrame)

	select {
	case <-fake.videoRequests:
		t.Fatal("video requested before the intercom started its own stream")
	case <-time.After(100 * time.Millisecond):
	}

	watch.observeFrame(testIntercomStreamFrame)

	select {
	case <-fake.videoRequests:
	case <-time.After(time.Second):
		t.Fatal("video not requested after the intercom stream started")
	}

	if err := <-done; err != nil {
		t.Fatalf("StartVideo() error = %v", err)
	}

	clip := watch.clip(nopRingClip{})
	clip.VideoRTP(&rtp.Packet{})
	clip.VideoRTP(&rtp.Packet{})

	out := logs.String()
	if strings.Count(out, "ring video timeline") != 1 {
		t.Fatalf("want one timeline line, got:\n%s", out)
	}

	for _, want := range []string{"reason=complete", "request_reason=intercom_stream"} {
		if !strings.Contains(out, want) {
			t.Fatalf("timeline lacks %q:\n%s", want, out)
		}
	}
}

func TestReactiveRingVideoFallsBack(t *testing.T) {
	watch, logs := newTestRingWatch(50 * time.Millisecond)
	fake := &fakeRingAV{videoRequests: make(chan time.Time, 1)}

	watch.observeFrame(testRingFrame)
	started := time.Now()

	if err := <-startVideoAsync(watch.sourceAV(fake)); err != nil {
		t.Fatalf("StartVideo() error = %v", err)
	}

	if requested := <-fake.videoRequests; requested.Sub(started) < 50*time.Millisecond {
		t.Fatalf("video requested after %v, before the fallback", requested.Sub(started))
	}

	watch.clip(nopRingClip{}).VideoRTP(&rtp.Packet{})

	if out := logs.String(); !strings.Contains(out, "request_reason=fallback") {
		t.Fatalf("timeline lacks the fallback:\n%s", out)
	}
}

func TestReactiveRingVideoGoesAtOnceWhenTheStreamIsAlreadyUp(t *testing.T) {
	watch, _ := newTestRingWatch(5 * time.Second)
	fake := &fakeRingAV{videoRequests: make(chan time.Time, 1)}

	watch.observeFrame(testRingFrame)
	watch.observeFrame(testIntercomStreamFrame)

	select {
	case err := <-startVideoAsync(watch.sourceAV(fake)):
		if err != nil {
			t.Fatalf("StartVideo() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("a preview opened after the intercom stream waited")
	}
}

func TestReactiveRingVideoWaitsAgainOnTheNextRing(t *testing.T) {
	watch, _ := newTestRingWatch(5 * time.Second)
	fake := &fakeRingAV{videoRequests: make(chan time.Time, 1)}

	watch.observeFrame(testRingFrame)
	watch.observeFrame(testIntercomStreamFrame)
	watch.observeFrame(testRingFrame)

	done := startVideoAsync(watch.sourceAV(fake))

	select {
	case <-fake.videoRequests:
		t.Fatal("the previous ring's stream released the request")
	case <-time.After(100 * time.Millisecond):
	}

	watch.observeFrame(testIntercomStreamFrame)

	if err := <-done; err != nil {
		t.Fatalf("StartVideo() error = %v", err)
	}
}

type nopRingClip struct{}

func (nopRingClip) VideoRTP(*rtp.Packet) {}
func (nopRingClip) Close()               {}
