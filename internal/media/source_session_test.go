package media

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSourceSessionStartsSIPThenAVOnlyOnceAndClosesEverything(t *testing.T) {
	sip := &fakeSourceSIP{}
	av := &fakeSourceAV{}
	video, audio := &fakeSourceReceiver{}, &fakeSourceReceiver{}
	session := NewSourceSession(nil, Profile{Model: "C300X", HighResVideo: true}, "main", "20", sip, av, video, audio)
	if err := session.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if sip.startCalls != 1 || av.calls != 1 || !av.highRes || video.starts != 1 || audio.starts != 1 {
		t.Fatalf("sip=%#v av=%#v video=%#v audio=%#v", sip, av, video, audio)
	}
	if err := session.Start(context.Background()); !errors.Is(err, ErrSourceSessionStarted) {
		t.Fatalf("second start error = %v", err)
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if sip.hangups != 1 || video.closes != 1 || audio.closes != 1 {
		t.Fatalf("cleanup sip=%#v video=%#v audio=%#v", sip, video, audio)
	}
}

func TestSourceSessionCleansUpSIPAndReceiversWhenAVFails(t *testing.T) {
	sip := &fakeSourceSIP{}
	av := &fakeSourceAV{err: errors.New("nack")}
	video, audio := &fakeSourceReceiver{}, &fakeSourceReceiver{}
	session := NewSourceSession(nil, Profile{Model: "C100X"}, "main", "20", sip, av, video, audio)
	err := session.Start(context.Background())
	if err == nil || sip.hangups != 1 || video.closes != 1 || audio.closes != 1 {
		t.Fatalf("start error=%v sip=%#v video=%#v audio=%#v", err, sip, video, audio)
	}
}

func TestSourceSessionRejectsMismatchedModelProfile(t *testing.T) {
	session := NewSourceSession(nil, Profile{Model: "C100X", HighResVideo: true}, "main", "20", &fakeSourceSIP{}, &fakeSourceAV{}, &fakeSourceReceiver{}, &fakeSourceReceiver{})
	if err := session.Start(context.Background()); !errors.Is(err, ErrUnsupportedModel) {
		t.Fatalf("start error = %v, want unsupported model", err)
	}
}

type fakeSourceSIP struct {
	startCalls int
	hangups    int
}

func (s *fakeSourceSIP) StartStream(context.Context, string) error { s.startCalls++; return nil }
func (s *fakeSourceSIP) Hangup(context.Context) error              { s.hangups++; return nil }

type fakeSourceAV struct {
	calls   int
	highRes bool
	err     error
}

func (a *fakeSourceAV) Start(_ context.Context, highRes bool, _, _ FlowProbe) error {
	a.calls++
	a.highRes = highRes
	return a.err
}

type fakeSourceReceiver struct {
	starts int
	closes int
}

func (r *fakeSourceReceiver) Start(context.Context) error      { r.starts++; return nil }
func (r *fakeSourceReceiver) Close() error                     { r.closes++; return nil }
func (*fakeSourceReceiver) RecentlyFlowing(time.Duration) bool { return false }
func (*fakeSourceReceiver) Metadata() RTPMetadata              { return RTPMetadata{} }
