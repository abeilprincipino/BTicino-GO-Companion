package media

import (
	"context"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func TestAudioBridge_ForwardsAudioAcrossMediaAndBackchannel(t *testing.T) {
	distributor := NewDistributor()
	intercom := testSource()
	intercom.MediaKind = MediaKindAudio
	intercom.SSRC = 4001
	opus := intercom
	opus.MediaKind = MediaKindAudioOpus
	opus.SSRC = 4002
	opus.Generation = "audio-bridge"
	if err := distributor.RegisterSource(intercom); err != nil {
		t.Fatal(err)
	}
	pipeline := &fakeAudioPipeline{
		opusOut:  make(chan *rtp.Packet, 1),
		speexOut: make(chan *rtp.Packet, 1),
	}
	backchannel := &recordingBackchannel{written: make(chan struct{}, 1)}
	bridge := NewAudioBridge(distributor, fakeGStreamerAudio{pipeline: pipeline}, intercom, opus, backchannel, "audio-bridge")
	if err := bridge.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer bridge.Stop()

	if !distributor.Distribute(intercom, testRTPPacket(intercom.SSRC)) {
		t.Fatal("Distribute() = false, want true")
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

	received := make(chan Packet, 1)
	if err := distributor.RegisterConsumer("opus", ConsumerFunc(func(packet Packet) {
		received <- packet
	})); err != nil {
		t.Fatal(err)
	}
	pipeline.opusOut <- testRTPPacket(opus.SSRC)
	if packet := <-received; packet.Source != opus {
		t.Fatalf("opus source = %#v, want %#v", packet.Source, opus)
	}
	pipeline.speexOut <- testRTPPacket(5002)
	select {
	case <-backchannel.written:
	case <-time.After(time.Second):
		t.Fatal("backchannel packet was not written")
	}
}

func TestAudioBridge_StartRejectsMissingDependencies(t *testing.T) {
	bridge := NewAudioBridge(nil, nil, Source{}, Source{}, nil, "")
	if err := bridge.Start(context.Background()); err != ErrAudioBridgeUnavailable {
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
