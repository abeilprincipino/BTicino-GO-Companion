package media

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func TestAudioBridge_ForwardsAudioAcrossMediaAndBackchannel(t *testing.T) {
	t.Parallel()

	pipeline := &fakeAudioPipeline{
		opusOut:  make(chan *rtp.Packet, 1),
		speexOut: make(chan *rtp.Packet, 1),
	}
	backchannel := &recordingBackchannel{written: make(chan struct{}, 1)}
	opus := make(chan *rtp.Packet, 1)

	bridge := NewAudioBridge(fakeGStreamerAudio{pipeline: pipeline}, func(packet *rtp.Packet) { opus <- packet }, backchannel, nil)
	if err := bridge.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer bridge.Stop() //nolint:errcheck // test cleanup

	if err := bridge.WriteIntercomSpeex(testRTPPacket(4001)); err != nil {
		t.Fatal(err)
	}
	if pipeline.intercomCalls != 1 {
		t.Fatalf("intercom calls = %d, want 1", pipeline.intercomCalls)
	}

	if err := bridge.WriteRTP(testRTPPacket(5001)); err != nil {
		t.Fatal(err)
	}

	if pipeline.backchannelCalls != 1 {
		t.Fatalf("backchannel calls = %d, want 1", pipeline.backchannelCalls)
	}

	pipeline.opusOut <- testRTPPacket(4002)
	if packet := <-opus; packet.SSRC != 4002 {
		t.Fatalf("opus packet SSRC = %d, want 4002", packet.SSRC)
	}

	pipeline.speexOut <- testRTPPacket(5002)

	select {
	case <-backchannel.written:
	case <-time.After(time.Second):
		t.Fatal("backchannel packet was not written")
	}
}

func TestAudioBridge_StartRejectsMissingDependencies(t *testing.T) {
	t.Parallel()

	bridge := NewAudioBridge(nil, nil, nil, nil)
	if err := bridge.Start(context.Background()); !errors.Is(err, ErrAudioBridgeUnavailable) {
		t.Fatalf("Start() error = %v, want %v", err, ErrAudioBridgeUnavailable)
	}
}

type fakeGStreamerAudio struct {
	pipeline AudioPipeline
}

func (g fakeGStreamerAudio) StartAudioBridge(context.Context) (AudioPipeline, error) {
	return g.pipeline, nil
}

type fakeAudioPipeline struct {
	intercomCalls    int
	backchannelCalls int
	opusOut          chan *rtp.Packet
	speexOut         chan *rtp.Packet
	closed           bool
}

type recordingBackchannel struct {
	calls   int
	written chan struct{}
}

func (b *recordingBackchannel) WriteRTP(*rtp.Packet) error {
	b.calls++
	b.written <- struct{}{}

	return nil
}

func (p *fakeAudioPipeline) WriteIntercomSpeex(*rtp.Packet) error {
	p.intercomCalls++
	return nil
}

func (p *fakeAudioPipeline) WriteBackchannelOpus(*rtp.Packet) error {
	p.backchannelCalls++
	return nil
}

func (p *fakeAudioPipeline) ReadOpusOut() <-chan *rtp.Packet {
	return p.opusOut
}

func (p *fakeAudioPipeline) ReadSpeexOut() <-chan *rtp.Packet {
	return p.speexOut
}

func (p *fakeAudioPipeline) Close() error {
	p.closed = true
	return nil
}
