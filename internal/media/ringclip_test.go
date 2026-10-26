package media

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/pmp4"
	"github.com/pion/rtp"
)

var (
	// A 1920x1080 baseline SPS and its PPS, from mediacommon's own tests: the
	// MP4 writer parses the SPS, so it must be real.
	testClipSPS = []byte{
		0x67, 0x42, 0xc0, 0x28, 0xd9, 0x00, 0x78, 0x02,
		0x27, 0xe5, 0x84, 0x00, 0x00, 0x03, 0x00, 0x04,
		0x00, 0x00, 0x03, 0x00, 0xf0, 0x3c, 0x60, 0xc9, 0x20,
	}
	testClipPPS   = []byte{0x08, 0x06, 0x07, 0x08}
	testClipIDR   = []byte{0x65, 0x88, 0x84, 0x00, 0x33}
	testClipSlice = []byte{0x41, 0x9a, 0x02, 0x04}
)

// testClipFrame is 1/30 s at 90 kHz.
const testClipFrame = 3000

func newTestClipWriter(t *testing.T) *clipWriter {
	t.Helper()

	writer, err := newClipWriter()
	if err != nil {
		t.Fatal(err)
	}

	return writer
}

// startTestClip feeds the parameter sets and an IDR at timestamp.
func startTestClip(t *testing.T, writer *clipWriter, timestamp uint32) {
	t.Helper()

	if writer.accessUnit(timestamp, [][]byte{testClipSPS, testClipPPS, testClipIDR}) {
		t.Fatal("the clip ended on its first access unit")
	}

	if !writer.started {
		t.Fatal("the clip did not start on SPS, PPS and IDR")
	}
}

func TestClipWriterWaitsForParameterSetsAndAnIDR(t *testing.T) {
	writer := newTestClipWriter(t)

	writer.accessUnit(0, [][]byte{testClipSlice})
	writer.accessUnit(3000, [][]byte{testClipIDR})

	if writer.started {
		t.Fatal("started without SPS and PPS")
	}

	writer.accessUnit(6000, [][]byte{testClipSPS})
	writer.accessUnit(9000, [][]byte{testClipPPS, testClipSlice})

	if writer.started {
		t.Fatal("started without an IDR")
	}

	writer.accessUnit(12000, [][]byte{testClipIDR})

	if !writer.started || len(writer.samples) != 1 || writer.samples[0].pts != 0 || !writer.samples[0].idr {
		t.Fatalf("started = %v, samples = %+v; want one IDR sample at 0", writer.started, writer.samples)
	}
}

func TestClipWriterStopsAtFifteenSeconds(t *testing.T) {
	writer := newTestClipWriter(t)
	startTestClip(t, writer, 0)

	for timestamp := uint32(testClipFrame); timestamp < 15*ringClipTimescale; timestamp += testClipFrame {
		if writer.accessUnit(timestamp, [][]byte{testClipSlice}) {
			t.Fatalf("the clip ended early, at %d", timestamp)
		}
	}

	if !writer.accessUnit(15*ringClipTimescale, [][]byte{testClipSlice}) {
		t.Fatal("the clip did not end at 15 s")
	}

	if len(writer.samples) != 450 {
		t.Fatalf("samples = %d, want 450: the access unit at 15 s is left out", len(writer.samples))
	}

	if got := writer.duration(); got != 15*time.Second {
		t.Fatalf("duration = %v, want 15s", got)
	}
}

func TestClipWriterEndsWhenTheSPSChanges(t *testing.T) {
	writer := newTestClipWriter(t)
	startTestClip(t, writer, 0)

	other := bytes.Clone(testClipSPS)
	other[len(other)-1] ^= 0xff

	if !writer.accessUnit(testClipFrame, [][]byte{other, testClipPPS, testClipIDR}) {
		t.Fatal("the clip did not end on a new SPS")
	}

	if len(writer.samples) != 1 {
		t.Fatalf("samples = %d, want 1", len(writer.samples))
	}
}

func TestClipWriterCapsTheClipSize(t *testing.T) {
	writer := newTestClipWriter(t)
	startTestClip(t, writer, 0)

	big := make([]byte, 1<<20)
	big[0] = 0x41

	ended := false
	for i := 1; i <= 10 && !ended; i++ {
		ended = writer.accessUnit(uint32(i*testClipFrame), [][]byte{big}) // #nosec G115 -- small test values.
	}

	if !ended {
		t.Fatal("the clip did not end at the size cap")
	}

	if writer.size > ringClipMaxBytes || len(writer.samples) != 8 {
		t.Fatalf("size = %d, samples = %d; want at most %d bytes in 8 samples", writer.size, len(writer.samples), ringClipMaxBytes)
	}
}

func TestClipWriterHandlesATimestampWrap(t *testing.T) {
	writer := newTestClipWriter(t)
	first := uint32(0xffffffff - 1000)
	startTestClip(t, writer, first)

	writer.accessUnit(first+testClipFrame, [][]byte{testClipSlice})

	if len(writer.samples) != 2 || writer.samples[1].pts != testClipFrame {
		t.Fatalf("samples = %+v, want the second at %d", writer.samples, testClipFrame)
	}
}

func TestClipWriterDropsAnAccessUnitThatGoesBack(t *testing.T) {
	writer := newTestClipWriter(t)
	startTestClip(t, writer, 9000)

	writer.accessUnit(6000, [][]byte{testClipSlice})
	writer.accessUnit(9000, [][]byte{testClipSlice})

	if len(writer.samples) != 1 {
		t.Fatalf("samples = %d, want 1", len(writer.samples))
	}
}

func TestClipWriterDepacketizesAggregatedAndFragmentedPackets(t *testing.T) {
	writer := newTestClipWriter(t)

	stapA := []byte{0x78}
	for _, nalu := range [][]byte{testClipSPS, testClipPPS} {
		stapA = append(stapA, byte(len(nalu)>>8), byte(len(nalu))) // #nosec G115 -- NAL sizes fit in 16 bits.
		stapA = append(stapA, nalu...)
	}

	// FU-A: indicator 0x7c (NRI 3, type 28), header 0x85 (start, IDR), 0x45 (end, IDR).
	fuStart := append([]byte{0x7c, 0x85}, testClipIDR[1:3]...)
	fuEnd := append([]byte{0x7c, 0x45}, testClipIDR[3:]...)

	packets := []*rtp.Packet{
		{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: 1}, Payload: stapA},
		{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: 2}, Payload: fuStart},
		{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: 3, Marker: true}, Payload: fuEnd},
	}

	for _, packet := range packets {
		if writer.push(packet) {
			t.Fatal("the clip ended while starting")
		}
	}

	want := append([]byte{0, 0, 0, byte(len(testClipIDR))}, testClipIDR...)
	if !writer.started || len(writer.samples) != 1 || !bytes.Equal(writer.samples[0].payload, want) {
		t.Fatalf("started = %v, samples = %+v; want the reassembled IDR", writer.started, writer.samples)
	}
}

func TestClipWriterOneSampleLastsOneFrame(t *testing.T) {
	writer := newTestClipWriter(t)
	startTestClip(t, writer, 0)

	if got, want := writer.duration(), ticksToDuration(ringClipDefaultSampleDuration); got != want {
		t.Fatalf("duration = %v, want %v", got, want)
	}
}

func TestClipWriterMarshalsAPlayableMP4(t *testing.T) {
	writer := newTestClipWriter(t)
	startTestClip(t, writer, 0)

	for i := 1; i < 60; i++ {
		writer.accessUnit(uint32(i*testClipFrame), [][]byte{testClipSlice}) // #nosec G115 -- small test values.
	}

	data, err := writer.marshal()
	if err != nil {
		t.Fatal(err)
	}

	var presentation pmp4.Presentation
	if err := presentation.Unmarshal(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}

	if len(presentation.Tracks) != 1 {
		t.Fatalf("tracks = %d, want 1", len(presentation.Tracks))
	}

	track := presentation.Tracks[0]

	codec, ok := track.Codec.(*codecs.H264)
	if !ok || !bytes.Equal(codec.SPS, testClipSPS) || !bytes.Equal(codec.PPS, testClipPPS) {
		t.Fatalf("codec = %#v, want H.264 with the stream's SPS and PPS", track.Codec)
	}

	if track.TimeScale != ringClipTimescale || len(track.Samples) != 60 || track.Samples[0].IsNonSyncSample || !track.Samples[1].IsNonSyncSample {
		t.Fatalf("timescale = %d, samples = %d; want 90000 and 60, the first one a sync sample", track.TimeScale, len(track.Samples))
	}
}

// feedTestClip sends an IDR with its parameter sets and then frames-1 slices,
// one every interval ticks.
func feedTestClip(clip *RingClip, frames int, interval uint32) {
	var sequence uint16

	send := func(timestamp uint32, marker bool, payload []byte) {
		sequence++
		clip.VideoRTP(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: sequence, Timestamp: timestamp, Marker: marker}, Payload: payload})
	}

	send(0, false, testClipSPS)
	send(0, false, testClipPPS)
	send(0, true, testClipIDR)

	for i := 1; i < frames; i++ {
		send(uint32(i)*interval, true, testClipSlice) // #nosec G115 -- small test values.
	}
}

func waitClipDone(t *testing.T, clip *RingClip) {
	t.Helper()

	select {
	case <-clip.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the clip never finished")
	}
}

func TestRingClipRecorderSavesTheClipOnClose(t *testing.T) {
	recorder := NewRingClipRecorder(t.TempDir(), nil)
	saved := make(chan struct{}, 1)
	recorder.SetOnSaved(func() { saved <- struct{}{} })

	before := time.Now().Add(-time.Second)
	clip := recorder.Start("main")
	feedTestClip(clip, 60, testClipFrame)
	clip.Close()
	waitClipDone(t, clip)

	select {
	case <-saved:
	default:
		t.Fatal("onSaved was not called")
	}

	file, recordedAt, err := recorder.Open("main")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close() //nolint:errcheck // test cleanup

	if recordedAt.Before(before) || recordedAt.After(time.Now()) || !recorder.RecordedAt("main").Equal(recordedAt) {
		t.Fatalf("recorded at %v, want between %v and now, and RecordedAt to match", recordedAt, before)
	}

	var presentation pmp4.Presentation
	if err := presentation.Unmarshal(file); err != nil {
		t.Fatal(err)
	}

	if len(presentation.Tracks) != 1 || len(presentation.Tracks[0].Samples) != 60 {
		t.Fatalf("saved clip has %d tracks, want 1 with 60 samples", len(presentation.Tracks))
	}
}

func TestRingClipRecorderFinishesByItselfAtFifteenSeconds(t *testing.T) {
	recorder := NewRingClipRecorder(t.TempDir(), nil)
	saved := make(chan struct{}, 1)
	recorder.SetOnSaved(func() { saved <- struct{}{} })

	clip := recorder.Start("main")
	// One frame a second: the 16th, at 15 s, ends the clip without a Close.
	feedTestClip(clip, 17, ringClipTimescale)
	waitClipDone(t, clip)

	select {
	case <-saved:
	default:
		t.Fatal("onSaved was not called")
	}
}

func TestRingClipRecorderKeepsThePreviousClipWhenTooShort(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clips", "ring", "main.mp4")

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	recorder := NewRingClipRecorder(dir, nil)
	saved := make(chan struct{}, 1)
	recorder.SetOnSaved(func() { saved <- struct{}{} })

	clip := recorder.Start("main")
	feedTestClip(clip, 15, testClipFrame) // half a second
	clip.Close()
	waitClipDone(t, clip)

	if data, err := os.ReadFile(path); err != nil || string(data) != "old" { // #nosec G304 -- test path.
		t.Fatalf("clip file = %q, %v; want the previous one kept", data, err)
	}

	select {
	case <-saved:
		t.Fatal("onSaved called for a discarded clip")
	default:
	}
}

func TestRingClipRecorderStartClosesThePreviousClip(t *testing.T) {
	recorder := NewRingClipRecorder(t.TempDir(), nil)

	first := recorder.Start("main")
	second := recorder.Start("main")

	waitClipDone(t, first)
	second.Close()
	waitClipDone(t, second)
}

func TestRingClipNilAndInvalidAreHarmless(t *testing.T) {
	var clip *RingClip
	clip.VideoRTP(&rtp.Packet{})
	clip.Close()

	var nilRecorder *RingClipRecorder
	if nilRecorder.Start("main") != nil {
		t.Fatal("a nil recorder started a clip")
	}

	recorder := NewRingClipRecorder(t.TempDir(), nil)
	if recorder.Start("../main") != nil {
		t.Fatal("started a clip for an id that leaves its directory")
	}

	if _, _, err := recorder.Open("main"); !errors.Is(err, ErrRingClipNotFound) {
		t.Fatalf("Open error = %v, want ErrRingClipNotFound", err)
	}

	if !recorder.RecordedAt("main").IsZero() {
		t.Fatal("RecordedAt is set with no clip")
	}
}

func TestNewRingClipRecorderRemovesLeftovers(t *testing.T) {
	dir := t.TempDir()
	leftover := filepath.Join(dir, "clips", "ring", ".ringclip-123.mp4")

	if err := os.MkdirAll(filepath.Dir(leftover), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(leftover, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	NewRingClipRecorder(dir, nil)

	if _, err := os.Stat(leftover); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("leftover still there: %v", err)
	}
}
