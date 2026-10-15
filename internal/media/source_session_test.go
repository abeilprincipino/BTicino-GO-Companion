package media

import (
	"bticino-go-companion/internal/core"
	"bticino-go-companion/internal/signaling"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The manager is the production SourceSIP. Asserting it here is what keeps a
// new lifecycle method on the interface from being added without the manager —
// the only implementation that owns a real dialog — growing one too.
var _ SourceSIP = (*signaling.Manager)(nil)

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

	// Once, not twice: the second call finds the session already stopped, and
	// the dialog it would drop is by then somebody else's.
	if sip.remoteEndeds != 1 {
		t.Fatalf("sip remote dialog notifications = %d, want 1", sip.remoteEndeds)
	}

	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("close after remote dialog ended: %v", err)
	}

	if sip.hangups != 0 {
		t.Fatalf("BYE count = %d, want 0", sip.hangups)
	}

	if sip.remoteEndeds != 1 {
		t.Fatalf("sip remote dialog notifications after close = %d, want 1", sip.remoteEndeds)
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

	// A local teardown sends its own BYE, so the SIP layer learns of the dialog
	// through Hangup and must not be told twice.
	if sip.hangups != 1 || sip.remoteEndeds != 0 || video.closes != 1 || audio.closes != 1 {
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

func TestSourceSessionPassesBoundReceiverPortsToAVClient(t *testing.T) {
	sip := &fakeSourceSIP{}
	av := &fakeSourceAV{}
	video := &fakeSourceReceiver{port: 41007}
	audio := &fakeSourceReceiver{port: 41000}

	session := NewSourceSession(nil, SourceConfig{Model: "C300X", DevAddr: "20"}, "main", sip, av, video, audio)
	if err := session.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	if want := (AVPorts{Video: 41007, Audio: 41000}); av.ports != want {
		t.Fatalf("av ports = %#v, want %#v", av.ports, want)
	}
}

func TestSourceSessionRejectsZeroBoundVideoPort(t *testing.T) {
	sip := &fakeSourceSIP{}
	av := &fakeSourceAV{}
	video := &zeroPortSourceReceiver{}
	audio := &fakeSourceReceiver{port: 41000}

	session := NewSourceSession(nil, SourceConfig{Model: "C300X", DevAddr: "20"}, "main", sip, av, video, audio)
	if err := session.Start(context.Background()); err == nil {
		t.Fatal("start succeeded with a zero-value bound video port")
	}

	if av.calls != 0 {
		t.Fatalf("av calls = %d, want 0 — must not advertise port 0 to the intercom", av.calls)
	}
}

func TestSourceSessionRejectsZeroBoundAudioPort(t *testing.T) {
	sip := &fakeSourceSIP{}
	av := &fakeSourceAV{}
	video := &fakeSourceReceiver{port: 41007}
	audio := &zeroPortSourceReceiver{}

	session := NewSourceSession(nil, SourceConfig{Model: "C300X", DevAddr: "20"}, "main", sip, av, video, audio)
	if err := session.Start(context.Background()); err == nil {
		t.Fatal("start succeeded with a zero-value bound audio port")
	}

	if av.calls != 0 {
		t.Fatalf("av calls = %d, want 0 — must not advertise port 0 to the intercom", av.calls)
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

	// The INVITE had already succeeded when the peer hung up, so the aborted
	// startup still owes the SIP layer the news that its dialog is gone.
	if sip.hangups != 0 || sip.remoteEndeds != 1 || video.closes != 1 || audio.closes != 1 {
		t.Fatalf("cleanup sip=%#v video=%#v audio=%#v", sip, video, audio)
	}
}

// TestSourceSessionRemoteDialogEndedClearsTheSharedSIPManager drives the real
// manager through the outbound preview's remote-BYE path.
//
// The session deliberately skips the BYE once the peer has ended the dialog, so
// Close never runs Hangup — and Hangup is one of only two things that clear
// Manager.active. The manager is a single process-wide instance shared by the
// API and every media source, so an active dialog it is never told about is
// permanent: every later preview fails with ErrActiveDialog and every later
// inbound INVITE is answered 486 Busy Here.
func TestSourceSessionRemoteDialogEndedClearsTheSharedSIPManager(t *testing.T) {
	dialer := &recordingStreamDialer{}
	manager := signaling.NewManager("192.0.2.10", dialer, nil, nil)
	video, audio := &fakeSourceReceiver{}, &fakeSourceReceiver{}

	session := NewSourceSession(nil, SourceConfig{Model: "C300X", DevAddr: "20"}, "main", manager, &fakeSourceAV{}, video, audio)
	if err := session.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	if dialer.startCalls() != 1 {
		t.Fatalf("preview invites = %d, want 1", dialer.startCalls())
	}

	session.RemoteDialogEnded()

	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("close after the remote dialog ended: %v", err)
	}

	if byes := dialer.byes(); byes != 0 {
		t.Fatalf("bye count = %d, want 0 — the far end already ended the dialog", byes)
	}

	// The manager must be free for the next lease. Before this was wired it
	// stayed active for the life of the process.
	if err := session.Start(context.Background()); err != nil {
		t.Fatalf("second start after a remote BYE: %v", err)
	}

	if dialer.startCalls() != 2 {
		t.Fatalf("preview invites = %d, want 2 — the manager still holds a dialog the far end tore down", dialer.startCalls())
	}
}

func TestSourceSessionFailedStartupDoesNotBYEAnAdoptedCall(t *testing.T) {
	sip := &fakeSourceSIP{adopted: true}
	av := &fakeSourceAV{err: errors.New("nack")}
	video, audio := &fakeSourceReceiver{}, &fakeSourceReceiver{}
	session := NewSourceSession(nil, SourceConfig{Model: "C100X", DevAddr: "20"}, "main", sip, av, video, audio)

	if err := session.Start(context.Background()); err == nil {
		t.Fatal("Start() error = nil, want the AV failure")
	}

	if sip.hangups != 0 {
		t.Fatalf("BYE count = %d, want 0 — the dialog belongs to the answered call, not to this session", sip.hangups)
	}

	if sip.remoteEndeds != 0 {
		t.Fatalf("remote dialog notifications = %d, want 0 — the peer did not end anything", sip.remoteEndeds)
	}

	if video.closes != 1 || audio.closes != 1 {
		t.Fatalf("receivers not released: video=%#v audio=%#v", video, audio)
	}
}

func TestSourceSessionCloseDoesNotBYEAnAdoptedCall(t *testing.T) {
	sip := &fakeSourceSIP{adopted: true}
	video, audio := &fakeSourceReceiver{}, &fakeSourceReceiver{}
	session := NewSourceSession(nil, SourceConfig{Model: "C300X", DevAddr: "20"}, "main", sip, &fakeSourceAV{}, video, audio)

	if err := session.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}

	if sip.hangups != 0 {
		t.Fatalf("BYE count = %d, want 0 — a media teardown must not end the call the user is on", sip.hangups)
	}

	if video.closes != 1 || audio.closes != 1 {
		t.Fatalf("receivers not released: video=%#v audio=%#v", video, audio)
	}
}

func TestSourceSessionCloseEndsAnAdoptedCallWhenTheConsumerReleased(t *testing.T) {
	sip := &fakeSourceSIP{adopted: true}
	session := NewSourceSession(nil, SourceConfig{Model: "C300X", DevAddr: "20"}, "main", sip, &fakeSourceAV{}, &fakeSourceReceiver{}, &fakeSourceReceiver{})

	if err := session.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	session.EndCallOnClose()

	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}

	if sip.hangups != 1 {
		t.Fatalf("BYE count = %d, want 1 — the user let go of the lease, which ends the doorbell call on purpose", sip.hangups)
	}
}

type recordingStreamDialer struct {
	mu      sync.Mutex
	calls   int
	dialogs []*recordingOutgoingDialog
}

func (d *recordingStreamDialer) StartStream(context.Context, string, string) (signaling.OutgoingDialog, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	dialog := &recordingOutgoingDialog{}
	d.calls++
	d.dialogs = append(d.dialogs, dialog)

	return dialog, nil
}

func (d *recordingStreamDialer) startCalls() int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.calls
}

func (d *recordingStreamDialer) byes() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()

	var total int64
	for _, dialog := range d.dialogs {
		total += dialog.byeCount.Load()
	}

	return total
}

type recordingOutgoingDialog struct{ byeCount atomic.Int64 }

func (d *recordingOutgoingDialog) Bye(context.Context) error {
	d.byeCount.Add(1)

	return nil
}

// recordingIncomingDialog is a ringing doorbell call as the manager sees it: it
// records every final or provisional response and every BYE.
type recordingIncomingDialog struct {
	id core.DialogID

	mu        sync.Mutex
	responses []int
	byes      int
}

func (d *recordingIncomingDialog) ID() core.DialogID { return d.id }

func (d *recordingIncomingDialog) Respond(_ context.Context, status int, _, _ string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.responses = append(d.responses, status)

	return nil
}

func (d *recordingIncomingDialog) Bye(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.byes++

	return nil
}

func (d *recordingIncomingDialog) statuses() []int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return append([]int(nil), d.responses...)
}

func (d *recordingIncomingDialog) byeCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.byes
}

// adopted makes StartStream report that it created no dialog, which is what the
// manager does during an answered inbound call. It defaults to false, so every
// test that does not opt in stands for an outbound preview that owns its dialog.
// adoptAnswered and adoptErr drive AdoptRingingCall for ringing previews.
type fakeSourceSIP struct {
	startCalls    int
	adoptCalls    int
	hangups       int
	remoteEndeds  int
	adopted       bool
	adoptAnswered bool
	adoptErr      error
}

func (s *fakeSourceSIP) StartStream(context.Context, string) (bool, error) {
	s.startCalls++

	return !s.adopted, nil
}

func (s *fakeSourceSIP) AdoptRingingCall(string) (bool, error) {
	s.adoptCalls++

	return s.adoptAnswered, s.adoptErr
}

func (s *fakeSourceSIP) HangupCall(context.Context) error { s.hangups++; return nil }
func (s *fakeSourceSIP) RemoteDialogEnded()               { s.remoteEndeds++ }

type blockingInviteSIP struct{ started chan struct{} }

func (s *blockingInviteSIP) StartStream(ctx context.Context, _ string) (bool, error) {
	close(s.started)
	<-ctx.Done()

	return false, fmt.Errorf("wait for invite answer: %w", ctx.Err())
}

func (*blockingInviteSIP) AdoptRingingCall(string) (bool, error) { return false, nil }
func (*blockingInviteSIP) HangupCall(context.Context) error      { return nil }
func (*blockingInviteSIP) RemoteDialogEnded()                    {}

type fakeSourceAV struct {
	calls      int
	highRes    bool
	ports      AVPorts
	err        error
	videoCalls int
	audioCalls int
	audioPort  int
	audioErr   error
}

func (a *fakeSourceAV) Start(_ context.Context, highRes bool, ports AVPorts, _, _ FlowProbe) error {
	a.calls++
	a.highRes = highRes
	a.ports = ports

	return a.err
}

func (a *fakeSourceAV) StartVideo(_ context.Context, highRes bool, port int, _ FlowProbe) error {
	a.videoCalls++
	a.highRes = highRes
	a.ports.Video = port

	return a.err
}

func (a *fakeSourceAV) StartAudio(_ context.Context, port int, _ FlowProbe) error {
	a.audioCalls++
	a.audioPort = port

	return a.audioErr
}

type blockingSourceAV struct{ started chan struct{} }

func (a *blockingSourceAV) Start(ctx context.Context, _ bool, _ AVPorts, _, _ FlowProbe) error {
	close(a.started)
	<-ctx.Done()

	return ctx.Err()
}

func (a *blockingSourceAV) StartVideo(ctx context.Context, _ bool, _ int, _ FlowProbe) error {
	close(a.started)
	<-ctx.Done()

	return ctx.Err()
}

func (*blockingSourceAV) StartAudio(context.Context, int, FlowProbe) error { return nil }

type uncooperativeSourceAV struct {
	started chan struct{}
	release chan struct{}
}

func (a *uncooperativeSourceAV) Start(context.Context, bool, AVPorts, FlowProbe, FlowProbe) error {
	close(a.started)
	<-a.release

	return context.Canceled
}

func (a *uncooperativeSourceAV) StartVideo(context.Context, bool, int, FlowProbe) error {
	close(a.started)
	<-a.release

	return context.Canceled
}

func (*uncooperativeSourceAV) StartAudio(context.Context, int, FlowProbe) error { return nil }

// fakeSourceReceiver stands in for a real RTPReceiver. Its Metadata().LocalPort
// defaults to a valid non-zero port when port is left unset, so the many
// pre-existing tests that don't care about port plumbing keep working; tests
// that do care set port explicitly. Use zeroPortSourceReceiver for the
// zero-port (unbound) case, which is deliberately distinct from "unset".
type fakeSourceReceiver struct {
	starts int
	closes int
	port   int
}

func (r *fakeSourceReceiver) Start(context.Context) error { r.starts++; return nil }
func (r *fakeSourceReceiver) Close() error                { r.closes++; return nil }
func (*fakeSourceReceiver) RecentlyFlowing(time.Duration) bool { return false }

func (r *fakeSourceReceiver) Metadata() RTPMetadata {
	port := r.port
	if port == 0 {
		port = 41999
	}

	return RTPMetadata{LocalPort: port}
}

// zeroPortSourceReceiver simulates a receiver that has not actually bound a
// port, reporting LocalPort 0 regardless of the embedded port field.
type zeroPortSourceReceiver struct{ fakeSourceReceiver }

func (*zeroPortSourceReceiver) Metadata() RTPMetadata { return RTPMetadata{LocalPort: 0} }

func TestSourceSessionRingingPreviewAdoptsWithoutDialingAndStartsVideoOnly(t *testing.T) {
	sip := &fakeSourceSIP{}
	av := &fakeSourceAV{}
	session := NewSourceSession(nil, SourceConfig{Model: "C300X", DevAddr: "20"}, "main", sip, av, &fakeSourceReceiver{port: 61007}, &fakeSourceReceiver{port: 61553})
	session.PrepareRingingPreview()

	if err := session.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	if sip.startCalls != 0 || sip.adoptCalls != 1 {
		t.Fatalf("sip start=%d adopt=%d, want 0 and 1 — a ringing preview never dials", sip.startCalls, sip.adoptCalls)
	}

	if av.videoCalls != 1 || av.calls != 0 || av.audioCalls != 0 || av.ports.Video != 61007 {
		t.Fatalf("av = %#v, want one video-only request on the bound port", av)
	}

	if !session.Ringing() {
		t.Fatal("Ringing() = false, want true")
	}
}

func TestSourceSessionRingingPreviewOfAnAnsweredCallStartsBothTracks(t *testing.T) {
	sip := &fakeSourceSIP{adoptAnswered: true}
	av := &fakeSourceAV{}
	session := NewSourceSession(nil, SourceConfig{Model: "C300X", DevAddr: "20"}, "main", sip, av, &fakeSourceReceiver{}, &fakeSourceReceiver{})
	session.PrepareRingingPreview()

	if err := session.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	if av.calls != 1 || av.videoCalls != 0 || session.Ringing() {
		t.Fatalf("av=%#v ringing=%v, want a full start — the call was answered before the preview got there", av, session.Ringing())
	}
}

func TestSourceSessionRingingPreviewFailsOnceTheRingIsOver(t *testing.T) {
	sip := &fakeSourceSIP{adoptErr: signaling.ErrNoInboundCall}
	av := &fakeSourceAV{}
	video, audio := &fakeSourceReceiver{}, &fakeSourceReceiver{}
	session := NewSourceSession(nil, SourceConfig{Model: "C300X", DevAddr: "20"}, "main", sip, av, video, audio)
	session.PrepareRingingPreview()

	if err := session.Start(context.Background()); !errors.Is(err, signaling.ErrNoInboundCall) {
		t.Fatalf("start error = %v, want ErrNoInboundCall", err)
	}

	if sip.startCalls != 0 || sip.hangups != 0 || av.videoCalls != 0 || av.calls != 0 {
		t.Fatalf("sip=%#v av=%#v, want no INVITE, no hangup, no AV request", sip, av)
	}

	if video.closes != 1 || audio.closes != 1 {
		t.Fatalf("receivers not released: video=%#v audio=%#v", video, audio)
	}
}

func TestSourceSessionAnsweredAsksForAudioOnTheBoundPort(t *testing.T) {
	sip := &fakeSourceSIP{}
	av := &fakeSourceAV{}
	session := NewSourceSession(nil, SourceConfig{Model: "C300X", DevAddr: "20"}, "main", sip, av, &fakeSourceReceiver{port: 61007}, &fakeSourceReceiver{port: 61553})
	session.PrepareRingingPreview()

	if err := session.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	if err := session.Answered(context.Background()); err != nil {
		t.Fatalf("Answered: %v", err)
	}

	if av.audioCalls != 1 || av.audioPort != 61553 || session.Ringing() {
		t.Fatalf("av=%#v ringing=%v, want one audio request on the port bound at start", av, session.Ringing())
	}

	if err := session.Answered(context.Background()); err != nil || av.audioCalls != 1 {
		t.Fatalf("second Answered err=%v audio calls=%d, want a no-op", err, av.audioCalls)
	}
}

func TestSourceSessionAnsweredFailureKeepsTheSessionRinging(t *testing.T) {
	sip := &fakeSourceSIP{}
	av := &fakeSourceAV{audioErr: errors.New("nack")}
	session := NewSourceSession(nil, SourceConfig{Model: "C300X", DevAddr: "20"}, "main", sip, av, &fakeSourceReceiver{}, &fakeSourceReceiver{})
	session.PrepareRingingPreview()

	if err := session.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	if err := session.Answered(context.Background()); err == nil {
		t.Fatal("Answered error = nil, want the audio failure")
	}

	if !session.Ringing() {
		t.Fatal("Ringing() = false after a failed promotion, want true")
	}
}

func TestSourceSessionAnsweredOnAnOrdinarySessionIsANoop(t *testing.T) {
	av := &fakeSourceAV{}
	session := NewSourceSession(nil, SourceConfig{Model: "C300X", DevAddr: "20"}, "main", &fakeSourceSIP{}, av, &fakeSourceReceiver{}, &fakeSourceReceiver{})

	if err := session.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	if err := session.Answered(context.Background()); err != nil || av.audioCalls != 0 {
		t.Fatalf("Answered err=%v audio calls=%d, want a no-op", err, av.audioCalls)
	}
}

// The invariant of docs/superpowers/specs/2026-10-15-ringing-preview-design.md,
// held against the real manager: whatever the session is told, its teardown
// during the ring sends neither a BYE nor a 603.
func TestSourceSessionRingingPreviewTeardownNeverRejectsTheCall(t *testing.T) {
	dialer := &recordingStreamDialer{}
	manager := signaling.NewManager("127.0.0.1", dialer, nil, func() (core.EntrypointID, string) { return "main", "20" })
	call := &recordingIncomingDialog{id: "call-1"}

	if err := manager.OnInvite(context.Background(), call); err != nil {
		t.Fatal(err)
	}

	session := NewSourceSession(nil, SourceConfig{Model: "C300X", DevAddr: "20"}, "main", manager, &fakeSourceAV{}, &fakeSourceReceiver{}, &fakeSourceReceiver{})
	session.PrepareRingingPreview()

	if err := session.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	// The consumer let go of the lease: the one teardown allowed to end a call.
	session.EndCallOnClose()

	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}

	if statuses := call.statuses(); len(statuses) != 1 || statuses[0] != 180 {
		t.Fatalf("responses = %v, want only 180 — a 603 would end every fork of the doorbell call", statuses)
	}

	if call.byeCount() != 0 || dialer.startCalls() != 0 {
		t.Fatalf("byes=%d invites=%d, want 0 and 0", call.byeCount(), dialer.startCalls())
	}

	if !manager.HasRingingInboundCall() {
		t.Fatal("HasRingingInboundCall() = false, want the call still ringing")
	}
}

func TestSourceSessionPromotedPreviewEndsTheCallWhenTheConsumerReleases(t *testing.T) {
	dialer := &recordingStreamDialer{}
	manager := signaling.NewManager("127.0.0.1", dialer, nil, func() (core.EntrypointID, string) { return "main", "20" })
	call := &recordingIncomingDialog{id: "call-1"}

	if err := manager.OnInvite(context.Background(), call); err != nil {
		t.Fatal(err)
	}

	session := NewSourceSession(nil, SourceConfig{Model: "C300X", DevAddr: "20"}, "main", manager, &fakeSourceAV{}, &fakeSourceReceiver{}, &fakeSourceReceiver{})
	session.PrepareRingingPreview()

	if err := session.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	if err := manager.Answer(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := session.Answered(context.Background()); err != nil {
		t.Fatalf("Answered: %v", err)
	}

	session.EndCallOnClose()

	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}

	if call.byeCount() != 1 {
		t.Fatalf("byes = %d, want 1 — the user let go of an answered call", call.byeCount())
	}
}
