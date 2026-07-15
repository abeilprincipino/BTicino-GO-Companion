package media

import (
	"bticino-go-companion/internal/core"
	"context"
	"errors"
	"testing"
)

func TestSnapshotService_CaptureUsesActiveDistributorVideo(t *testing.T) {
	t.Parallel()

	distributor := NewDistributor()

	source := testSource()
	if err := distributor.RegisterSource(source); err != nil {
		t.Fatal(err)
	}

	capture := &fakeSnapshotCapture{image: []byte("jpeg")}
	capture.wait = func() {
		distributor.Distribute(source, testRTPPacket(source.SSRC))
	}
	openwebnet := &fakeOpenWebNetVideo{}
	service := NewSnapshotService(distributor, fakeGStreamerSnapshot{capture: capture}, openwebnet)

	image, err := service.Capture(context.Background(), source.EntrypointID)
	if err != nil {
		t.Fatal(err)
	}

	if string(image) != "jpeg" {
		t.Fatalf("Capture() = %q, want jpeg", image)
	}

	if openwebnet.calls != 0 {
		t.Fatalf("OpenWebNet calls = %d, want 0", openwebnet.calls)
	}

	if !capture.closed {
		t.Fatal("capture was not closed")
	}

	if capture.packets != 1 {
		t.Fatalf("capture packets = %d, want 1", capture.packets)
	}
}

func TestSnapshotService_CaptureRequestsOpenWebNetVideoWhenIdle(t *testing.T) {
	t.Parallel()

	distributor := NewDistributor()
	entrypointID := core.EntrypointID("front-door")
	source := testSource()
	capture := &fakeSnapshotCapture{image: []byte("jpeg")}
	openwebnet := &fakeOpenWebNetVideo{start: func(context.Context, core.EntrypointID) error {
		return distributor.RegisterSource(source)
	}}

	service := NewSnapshotService(distributor, fakeGStreamerSnapshot{capture: capture}, openwebnet)
	if _, err := service.Capture(context.Background(), entrypointID); err != nil {
		t.Fatal(err)
	}

	if openwebnet.calls != 1 {
		t.Fatalf("OpenWebNet calls = %d, want 1", openwebnet.calls)
	}

	if openwebnet.entrypointID != entrypointID {
		t.Fatalf("OpenWebNet entrypoint = %q, want %q", openwebnet.entrypointID, entrypointID)
	}
}

func TestSnapshotService_CaptureRequiresDistributorVideoAfterOpenWebNetRequest(t *testing.T) {
	t.Parallel()

	service := NewSnapshotService(NewDistributor(), fakeGStreamerSnapshot{capture: &fakeSnapshotCapture{}}, &fakeOpenWebNetVideo{})
	if _, err := service.Capture(context.Background(), "front-door"); !errors.Is(err, ErrSnapshotNoVideo) {
		t.Fatalf("Capture() error = %v, want %v", err, ErrSnapshotNoVideo)
	}
}

type fakeGStreamerSnapshot struct {
	capture SnapshotCapture
}

func (g fakeGStreamerSnapshot) StartSnapshot(context.Context) (SnapshotCapture, error) {
	return g.capture, nil
}

type fakeSnapshotCapture struct {
	image   []byte
	closed  bool
	packets int
	wait    func()
}

func (c *fakeSnapshotCapture) Consume(Packet) {
	c.packets++
}

func (c *fakeSnapshotCapture) Wait(context.Context) ([]byte, error) {
	if c.wait != nil {
		c.wait()
	}

	return c.image, nil
}

func (c *fakeSnapshotCapture) Close() error {
	c.closed = true
	return nil
}

type fakeOpenWebNetVideo struct {
	calls        int
	entrypointID core.EntrypointID
	start        func(context.Context, core.EntrypointID) error
}

func (o *fakeOpenWebNetVideo) StartVideo(ctx context.Context, entrypointID core.EntrypointID) error {
	o.calls++

	o.entrypointID = entrypointID
	if o.start == nil {
		return nil
	}

	return o.start(ctx, entrypointID)
}
