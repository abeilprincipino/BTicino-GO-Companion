package media

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestSourceSessionStartsSIPThenAVOnlyOnceAndClosesEverything(t *testing.T) {
	sip := &fakeSourceSIP{}
	av := &fakeSourceAV{}
	video, audio := &fakeSourceReceiver{}, &fakeSourceReceiver{}

	session := NewSourceSession(nil, SourceConfig{Model: "C300X", DevAddr: "20", HighResVideo: true}, "main", sip, av, video, audio)
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
	session := NewSourceSession(nil, SourceConfig{Model: "C100X", DevAddr: "20"}, "main", sip, av, video, audio)

	err := session.Start(context.Background())
	if err == nil || sip.hangups != 1 || video.closes != 1 || audio.closes != 1 {
		t.Fatalf("start error=%v sip=%#v video=%#v audio=%#v", err, sip, video, audio)
	}
}

func TestSourceSessionRejectsIncompleteSourceConfig(t *testing.T) {
	session := NewSourceSession(nil, SourceConfig{Model: "C100X"}, "main", &fakeSourceSIP{}, &fakeSourceAV{}, &fakeSourceReceiver{}, &fakeSourceReceiver{})
	if err := session.Start(context.Background()); err == nil {
		t.Fatal("start succeeded with incomplete source config")
	}
}

func TestSourceSession_RemoteDialogEndedClosesReceiversWithoutSendingBYE(t *testing.T) {
	sip := &fakeSourceSIP{}
	video, audio := &fakeSourceReceiver{}, &fakeSourceReceiver{}

	session := NewSourceSession(nil, SourceConfig{Model: "C300X", DevAddr: "20"}, "main", sip, &fakeSourceAV{}, video, audio)
	if err := session.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	session.RemoteDialogEnded()
	session.RemoteDialogEnded()

	if session.started {
		t.Fatal("session remains started after remote dialog ends")
	}

	if sip.hangups != 0 || video.closes != 1 || audio.closes != 1 {
		t.Fatalf("remote cleanup sip=%#v video=%#v audio=%#v", sip, video, audio)
	}

	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("close after remote dialog ended: %v", err)
	}

	if sip.hangups != 0 {
		t.Fatalf("BYE count = %d, want 0", sip.hangups)
	}
}

func TestSourceSessionCloseCancelsStartupAndCleansUp(t *testing.T) {
	sip := &fakeSourceSIP{}
	av := &blockingSourceAV{started: make(chan struct{})}
	video, audio := &fakeSourceReceiver{}, &fakeSourceReceiver{}
	session := NewSourceSession(nil, SourceConfig{Model: "C300X", DevAddr: "20"}, "main", sip, av, video, audio)

	startResult := make(chan error, 1)
	go func() { startResult <- session.Start(context.Background()) }()

	select {
	case <-av.started:
	case <-time.After(time.Second):
		t.Fatal("source session did not reach AV startup")
	}

	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := <-startResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("start error = %v, want context cancellation", err)
	}

	if sip.hangups != 1 || video.closes != 1 || audio.closes != 1 {
		t.Fatalf("cleanup sip=%#v video=%#v audio=%#v", sip, video, audio)
	}
}

func TestSourceSessionCloseCancelsPendingSIPInvite(t *testing.T) {
	sip := &blockingInviteSIP{started: make(chan struct{})}
	video, audio := &fakeSourceReceiver{}, &fakeSourceReceiver{}
	session := NewSourceSession(nil, SourceConfig{Model: "C300X", DevAddr: "20"}, "main", sip, &fakeSourceAV{}, video, audio)

	startResult := make(chan error, 1)
	go func() { startResult <- session.Start(context.Background()) }()

	select {
	case <-sip.started:
	case <-time.After(time.Second):
		t.Fatal("source session did not reach SIP invite")
	}

	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := <-startResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("start error = %v, want context cancellation", err)
	}

	if video.closes != 1 || audio.closes != 1 {
		t.Fatalf("receivers after canceled invite: video=%#v audio=%#v", video, audio)
	}
}

func TestSourceSessionCloseReturnsWhenStartupIgnoresCancellation(t *testing.T) {
	sip := &fakeSourceSIP{}
	av := &uncooperativeSourceAV{started: make(chan struct{}), release: make(chan struct{})}
	video, audio := &fakeSourceReceiver{}, &fakeSourceReceiver{}
	session := NewSourceSession(nil, SourceConfig{Model: "C300X", DevAddr: "20"}, "main", sip, av, video, audio)

	startResult := make(chan error, 1)
	go func() { startResult <- session.Start(context.Background()) }()

	select {
	case <-av.started:
	case <-time.After(time.Second):
		t.Fatal("source session did not reach AV startup")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	if err := session.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close error = %v, want context deadline exceeded", err)
	}

	close(av.release)

	if err := <-startResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("start error = %v, want context cancellation", err)
	}
}

func TestSourceSessionRemoteDialogEndedDuringStartupDoesNotSendBYE(t *testing.T) {
	sip := &fakeSourceSIP{}
	av := &blockingSourceAV{started: make(chan struct{})}
	video, audio := &fakeSourceReceiver{}, &fakeSourceReceiver{}
	session := NewSourceSession(nil, SourceConfig{Model: "C300X", DevAddr: "20"}, "main", sip, av, video, audio)

	startResult := make(chan error, 1)
	go func() { startResult <- session.Start(context.Background()) }()

	select {
	case <-av.started:
	case <-time.After(time.Second):
		t.Fatal("source session did not reach AV startup")
	}

	session.RemoteDialogEnded()

	if err := <-startResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("start error = %v, want context cancellation", err)
	}

	if sip.hangups != 0 || video.closes != 1 || audio.closes != 1 {
		t.Fatalf("cleanup sip=%#v video=%#v audio=%#v", sip, video, audio)
	}
}

type fakeSourceSIP struct {
	startCalls int
	hangups    int
}

func (s *fakeSourceSIP) StartStream(context.Context, string) error { s.startCalls++; return nil }
func (s *fakeSourceSIP) Hangup(context.Context) error              { s.hangups++; return nil }

type blockingInviteSIP struct{ started chan struct{} }

func (s *blockingInviteSIP) StartStream(ctx context.Context, _ string) error {
	close(s.started)
	<-ctx.Done()

	return fmt.Errorf("wait for invite answer: %w", ctx.Err())
}

func (*blockingInviteSIP) Hangup(context.Context) error { return nil }

type fakeSourceAV struct {
	calls   int
	highRes bool
	err     error
}

type blockingSourceAV struct{ started chan struct{} }

func (a *blockingSourceAV) Start(ctx context.Context, _ bool, _, _ FlowProbe) error {
	close(a.started)
	<-ctx.Done()

	return ctx.Err()
}

type uncooperativeSourceAV struct {
	started chan struct{}
	release chan struct{}
}

func (a *uncooperativeSourceAV) Start(context.Context, bool, FlowProbe, FlowProbe) error {
	close(a.started)
	<-a.release

	return context.Canceled
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
