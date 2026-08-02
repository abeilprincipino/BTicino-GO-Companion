package media

import (
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

type fakeSourceSIP struct {
	startCalls   int
	hangups      int
	remoteEndeds int
}

func (s *fakeSourceSIP) StartStream(context.Context, string) error { s.startCalls++; return nil }
func (s *fakeSourceSIP) Hangup(context.Context) error              { s.hangups++; return nil }
func (s *fakeSourceSIP) RemoteDialogEnded()                        { s.remoteEndeds++ }

type blockingInviteSIP struct{ started chan struct{} }

func (s *blockingInviteSIP) StartStream(ctx context.Context, _ string) error {
	close(s.started)
	<-ctx.Done()

	return fmt.Errorf("wait for invite answer: %w", ctx.Err())
}

func (*blockingInviteSIP) Hangup(context.Context) error { return nil }
func (*blockingInviteSIP) RemoteDialogEnded()           {}

type fakeSourceAV struct {
	calls   int
	highRes bool
	ports   AVPorts
	err     error
}

type blockingSourceAV struct{ started chan struct{} }

func (a *blockingSourceAV) Start(ctx context.Context, _ bool, _ AVPorts, _, _ FlowProbe) error {
	close(a.started)
	<-ctx.Done()

	return ctx.Err()
}

type uncooperativeSourceAV struct {
	started chan struct{}
	release chan struct{}
}

func (a *uncooperativeSourceAV) Start(context.Context, bool, AVPorts, FlowProbe, FlowProbe) error {
	close(a.started)
	<-a.release

	return context.Canceled
}

func (a *fakeSourceAV) Start(_ context.Context, highRes bool, ports AVPorts, _, _ FlowProbe) error {
	a.calls++
	a.highRes = highRes
	a.ports = ports

	return a.err
}

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
