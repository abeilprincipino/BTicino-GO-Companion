package media

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pion/rtp"
)

var (
	ErrAudioBridgeUnavailable = errors.New("media: audio bridge is unavailable")
	ErrAudioBridgeStarted     = errors.New("media: audio bridge is already started")
)

const (
	audioBridgeSpeexIn           = 51060
	audioBridgePCMDown           = 51062
	audioBridgeOpusOut           = 51064
	audioBridgeOpusIn            = 51066
	audioBridgePCMUp             = 51068
	audioBridgeSpeexOut          = 51070
	audioBridgeOpusPT            = 111
	audioBridgeBackchannelOpusPT = 112
	audioBridgeRestartLimit      = 2
	audioBridgeStopTimeout       = 5 * time.Second
)

type AudioPipeline interface {
	WriteIntercomSpeex(*rtp.Packet) error
	WriteBackchannelOpus(*rtp.Packet) error
	ReadOpusOut() <-chan *rtp.Packet
	ReadSpeexOut() <-chan *rtp.Packet
	Errors() <-chan error
	Close() error
}

type GStreamerAudio interface {
	StartAudioBridge(context.Context) (AudioPipeline, error)
}

type AudioBridge struct {
	mu sync.Mutex

	gstreamer   GStreamerAudio
	opusOutput  func(*rtp.Packet)
	backchannel Backchannel
	logger      *slog.Logger
	failure     func(error)
	pipeline    AudioPipeline
	lifecycle   context.Context
	cancel      context.CancelFunc
	done        chan struct{}
	active      bool
	generation  uint64
	restarts    int
}

func NewAudioBridge(
	gstreamer GStreamerAudio,
	opusOutput func(*rtp.Packet),
	backchannel Backchannel,
	logger *slog.Logger,
	failure func(error),
) *AudioBridge {
	if logger == nil {
		logger = slog.Default()
	}
	return &AudioBridge{
		gstreamer:   gstreamer,
		opusOutput:  opusOutput,
		backchannel: backchannel,
		logger:      logger.With("component", "media.audio"),
		failure:     failure,
	}
}

func (b *AudioBridge) Start(ctx context.Context) error {
	if b == nil || b.gstreamer == nil || b.opusOutput == nil {
		return ErrAudioBridgeUnavailable
	}

	b.mu.Lock()

	if b.pipeline != nil {
		b.mu.Unlock()
		return ErrAudioBridgeStarted
	}
	b.active = true
	b.restarts = 0
	b.lifecycle = ctx
	b.mu.Unlock()

	b.logger.InfoContext(ctx, "audio bridge starting", "direction", "downlink")
	pipeline, err := b.gstreamer.StartAudioBridge(ctx)
	if err != nil {
		b.mu.Lock()
		b.active = false
		b.mu.Unlock()
		return err
	}

	if pipeline == nil {
		b.mu.Lock()
		b.active = false
		b.mu.Unlock()
		return ErrAudioBridgeUnavailable
	}
	b.mu.Lock()
	if !b.active {
		b.mu.Unlock()
		_ = pipeline.Close()
		return ErrAudioBridgeUnavailable
	}
	b.installLocked(pipeline)
	generation := b.generation
	b.mu.Unlock()
	b.logger.InfoContext(ctx, "audio bridge started", "generation", generation)

	return nil
}

func (b *AudioBridge) installLocked(pipeline AudioPipeline) {
	runCtx, cancel := context.WithCancel(context.Background())
	b.generation++
	b.pipeline = pipeline
	b.cancel = cancel
	b.done = make(chan struct{})
	go b.forward(runCtx, pipeline, b.done)
	go b.monitor(runCtx, pipeline)
}

func (b *AudioBridge) monitor(ctx context.Context, pipeline AudioPipeline) {
	select {
	case <-ctx.Done():
		return
	case err, ok := <-pipeline.Errors():
		if !ok || err == nil {
			return
		}
		b.restart(pipeline, err)
	}
}

func (b *AudioBridge) restart(failed AudioPipeline, cause error) {
	b.mu.Lock()
	if !b.active || b.pipeline != failed {
		b.mu.Unlock()
		return
	}
	if b.restarts >= audioBridgeRestartLimit {
		b.mu.Unlock()
		b.logger.Error("audio bridge restart budget exhausted", "generation", b.generation, "attempts", audioBridgeRestartLimit, "error", cause)
		if b.failure != nil {
			b.failure(cause)
		}
		return
	}
	b.restarts++
	attempt, generation := b.restarts, b.generation
	cancel, done := b.cancel, b.done
	b.pipeline, b.cancel, b.done = nil, nil, nil
	b.mu.Unlock()
	b.logger.Warn("audio bridge restarting", "generation", generation, "attempt", attempt, "limit", audioBridgeRestartLimit, "error", cause)
	cancel()
	_ = failed.Close()
	<-done

	b.mu.Lock()
	lifecycle := b.lifecycle
	b.mu.Unlock()
	if lifecycle == nil {
		lifecycle = context.Background()
	}
	pipeline, err := b.gstreamer.StartAudioBridge(lifecycle)
	if err != nil || pipeline == nil {
		if err == nil {
			err = ErrAudioBridgeUnavailable
		}
		b.logger.Warn("audio bridge restart failed", "generation", generation, "attempt", attempt, "error", err)
		b.restartFailure(failed, err)
		return
	}
	b.mu.Lock()
	if !b.active || b.pipeline != nil {
		b.mu.Unlock()
		_ = pipeline.Close()
		return
	}
	b.installLocked(pipeline)
	newGeneration := b.generation
	b.mu.Unlock()
	b.logger.Info("audio bridge restarted", "generation", newGeneration, "attempt", attempt)
}

func (b *AudioBridge) restartFailure(failed AudioPipeline, err error) {
	_ = failed
	b.logger.Error("audio bridge restart exhausted", "attempts", audioBridgeRestartLimit, "error", err)
	if b.failure != nil {
		b.failure(err)
	}
}

func (b *AudioBridge) WriteRTP(packet *rtp.Packet) error {
	return b.WriteBackchannelOpus(packet)
}

func (b *AudioBridge) WriteIntercomSpeex(packet *rtp.Packet) error {
	b.mu.Lock()
	pipeline := b.pipeline
	b.mu.Unlock()
	if pipeline == nil {
		return ErrAudioBridgeUnavailable
	}
	return pipeline.WriteIntercomSpeex(packet)
}

func (b *AudioBridge) WriteBackchannelOpus(packet *rtp.Packet) error {
	b.mu.Lock()
	pipeline := b.pipeline
	b.mu.Unlock()

	if pipeline == nil {
		return ErrAudioBridgeUnavailable
	}

	return pipeline.WriteBackchannelOpus(packet)
}

func (b *AudioBridge) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), audioBridgeStopTimeout)
	defer cancel()

	return b.StopContext(ctx)
}

// StopContext stops the bridge and waits for forwarding to exit until ctx expires.
// Pipeline Close is synchronous so its GStreamer children are reaped before this returns.
func (b *AudioBridge) StopContext(ctx context.Context) error {
	if b == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	b.mu.Lock()

	pipeline := b.pipeline
	b.active = false
	if pipeline == nil {
		b.mu.Unlock()
		return nil
	}
	b.logger.Info("audio bridge stopping")

	b.pipeline = nil
	b.lifecycle = nil
	cancel := b.cancel
	done := b.done
	b.cancel = nil
	b.done = nil
	b.mu.Unlock()

	cancel()

	err := pipeline.Close()

	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}

	if err != nil {
		return err
	}
	b.logger.Info("audio bridge stopped")
	return nil
}

func (b *AudioBridge) forward(ctx context.Context, pipeline AudioPipeline, done chan struct{}) {
	defer close(done)

	opusOut := pipeline.ReadOpusOut()

	speexOut := pipeline.ReadSpeexOut()
	for opusOut != nil || speexOut != nil {
		select {
		case <-ctx.Done():
			return
		case packet, ok := <-opusOut:
			if !ok {
				opusOut = nil
				continue
			}

			if packet != nil {
				b.opusOutput(packet)
			}
		case packet, ok := <-speexOut:
			if !ok {
				speexOut = nil
				continue
			}

			if packet != nil && b.backchannel != nil {
				_ = b.backchannel.WriteRTP(packet)
			}
		}
	}
}

// GStreamerAudioBridge is the process implementation of GStreamerAudio.
type GStreamerAudioBridge struct {
	bundleRoot string
	logger     *slog.Logger
}

func NewGStreamerAudioBridge(bundleRoot string, logger *slog.Logger) *GStreamerAudioBridge {
	if logger == nil {
		logger = slog.Default()
	}
	return &GStreamerAudioBridge{bundleRoot: bundleRoot, logger: logger.With("component", "media.audiobridge")}
}

func (g *GStreamerAudioBridge) StartAudioBridge(ctx context.Context) (AudioPipeline, error) {
	opusConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: audioBridgeOpusOut})
	if err != nil {
		return nil, fmt.Errorf("listen bridge opus output: %w", err)
	}
	speexConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: audioBridgeSpeexOut})
	if err != nil {
		_ = opusConn.Close()
		return nil, fmt.Errorf("listen bridge speex output: %w", err)
	}
	p := &gstreamerAudioPipeline{logger: g.logger, opusConn: opusConn, speexConn: speexConn, opusOut: make(chan *rtp.Packet, 32), speexOut: make(chan *rtp.Packet, 32), errors: make(chan error, 4), stop: make(chan struct{}), closeDone: make(chan struct{})}
	bundle := filepath.Join(g.bundleRoot, "bin", "gst-launch-1.0")
	p.commands = []*exec.Cmd{
		exec.CommandContext(ctx, "/usr/bin/gst-launch-1.0", "-q", "udpsrc", "port=51060", "caps=application/x-rtp,media=audio,encoding-name=SPEEX,clock-rate=8000,payload=110", "!", "rtpspeexdepay", "!", "speexdec", "!", "audioconvert", "!", "audioresample", "!", "audio/x-raw,format=S16BE,rate=8000,channels=1", "!", "rtpL16pay", "pt=96", "!", "udpsink", "host=127.0.0.1", "port=51062"),
		exec.CommandContext(ctx, bundle, "-q", "udpsrc", "port=51062", "caps=application/x-rtp,media=audio,encoding-name=L16,clock-rate=8000,channels=1,encoding-params=1,payload=96", "!", "rtpL16depay", "!", "audioconvert", "!", "audioresample", "!", "audio/x-raw,format=S16LE,rate=48000,channels=1", "!", "opusenc", "bitrate=24000", "complexity=5", "frame-size=20", "dtx=false", "inband-fec=false", "audio-type=voice", "!", "rtpopuspay", "pt=111", "!", "udpsink", "host=127.0.0.1", "port=51064"),
		exec.CommandContext(ctx, bundle, "-q", "udpsrc", "port=51066", "caps=application/x-rtp,media=audio,encoding-name=OPUS,clock-rate=48000,payload=112", "!", "rtpopusdepay", "!", "opusdec", "!", "audioconvert", "!", "audioresample", "!", "audio/x-raw,format=S16BE,rate=8000,channels=1", "!", "rtpL16pay", "pt=96", "!", "udpsink", "host=127.0.0.1", "port=51068"),
		exec.CommandContext(ctx, "/usr/bin/gst-launch-1.0", "-q", "udpsrc", "port=51068", "caps=application/x-rtp,media=audio,encoding-name=L16,clock-rate=8000,channels=1,encoding-params=1,payload=96", "!", "rtpL16depay", "!", "audioconvert", "!", "audioresample", "!", "audio/x-raw,rate=8000,channels=1", "!", "speexenc", "!", "rtpspeexpay", "pt=97", "!", "udpsink", "host=127.0.0.1", "port=51070"),
	}
	for index, command := range p.commands {
		// Each launch owns a process group so Close can also terminate descendants.
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if index == 1 || index == 2 {
			command.Env = bundledGSTEnv(g.bundleRoot)
		}
		if err := command.Start(); err != nil {
			_ = p.Close()
			return nil, fmt.Errorf("start audio bridge pipeline: %w", err)
		}
		g.logger.Debug("audio bridge pipeline started", "pipeline", audioPipelineName(index))
		p.waiters.Add(1)
		go p.wait(command, audioPipelineName(index))
	}
	p.readers.Add(2)
	go p.read(p.opusConn, audioBridgeOpusPT, p.opusOut)
	go p.read(p.speexConn, 97, p.speexOut)
	return p, nil
}

type gstreamerAudioPipeline struct {
	mu                                   sync.Mutex
	logger                               *slog.Logger
	commands                             []*exec.Cmd
	opusConn, speexConn, speexIn, opusIn *net.UDPConn
	opusOut, speexOut                    chan *rtp.Packet
	errors                               chan error
	waiters                              sync.WaitGroup
	closeDone                            chan struct{}
	stop                                 chan struct{}
	readers                              sync.WaitGroup
	closeErr                             error
	closed                               bool
}

func (p *gstreamerAudioPipeline) wait(command *exec.Cmd, name string) {
	defer p.waiters.Done()
	err := command.Wait()
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if !closed {
		if err == nil {
			err = fmt.Errorf("audio bridge pipeline %s exited unexpectedly", name)
		}
		select {
		case p.errors <- err:
		default:
		}
	}
}

func audioPipelineName(index int) string {
	switch index {
	case 0:
		return "downlink-speex-decode"
	case 1:
		return "downlink-opus-encode"
	case 2:
		return "uplink-opus-decode"
	case 3:
		return "uplink-speex-encode"
	default:
		return "unknown"
	}
}

func (p *gstreamerAudioPipeline) WriteIntercomSpeex(packet *rtp.Packet) error {
	return p.write(packet, &p.speexIn, audioBridgeSpeexIn)
}
func (p *gstreamerAudioPipeline) WriteBackchannelOpus(packet *rtp.Packet) error {
	return p.write(packet, &p.opusIn, audioBridgeOpusIn)
}
func (p *gstreamerAudioPipeline) ReadOpusOut() <-chan *rtp.Packet  { return p.opusOut }
func (p *gstreamerAudioPipeline) ReadSpeexOut() <-chan *rtp.Packet { return p.speexOut }
func (p *gstreamerAudioPipeline) Errors() <-chan error             { return p.errors }
func (p *gstreamerAudioPipeline) write(packet *rtp.Packet, conn **net.UDPConn, port int) error {
	if packet == nil {
		return nil
	}
	raw, err := packet.Marshal()
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrAudioBridgeUnavailable
	}
	if *conn == nil {
		*conn, err = net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
		if err != nil {
			return err
		}
	}
	_, err = (*conn).Write(raw)
	return err
}
func (p *gstreamerAudioPipeline) Close() error {
	p.mu.Lock()
	if p.closed {
		done := p.closeDone
		p.mu.Unlock()
		<-done
		return p.closeErr
	}
	p.closed = true
	if p.stop != nil {
		close(p.stop)
	}
	p.logger.Info("audio bridge pipeline stopping")
	for _, conn := range []*net.UDPConn{p.opusConn, p.speexConn, p.speexIn, p.opusIn} {
		if conn != nil {
			_ = conn.Close()
		}
	}
	for _, command := range p.commands {
		if command.Process != nil {
			// A negative PID targets the process group created at Start.
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		}
	}
	p.mu.Unlock()

	// Wait must be called exactly once per started command. The waiter goroutines
	// do that work and Close does not return until every child has been reaped.
	p.waiters.Wait()
	p.readers.Wait()

	p.mu.Lock()
	close(p.closeDone)
	p.mu.Unlock()
	return p.closeErr
}
func (p *gstreamerAudioPipeline) read(conn *net.UDPConn, payloadType uint8, output chan<- *rtp.Packet) {
	defer p.readers.Done()
	defer close(output)
	buffer := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		packet := &rtp.Packet{}
		if packet.Unmarshal(buffer[:n]) == nil && packet.PayloadType == payloadType {
			select {
			case output <- packet:
			case <-p.stop:
				return
			}
		}
	}
}

func bundledGSTEnv(bundleRoot string) []string {
	root := strings.TrimSpace(bundleRoot)
	libDir := filepath.Join(root, "lib")
	return []string{
		"PATH=/usr/bin:/bin",
		"LD_LIBRARY_PATH=" + libDir,
		"GST_PLUGIN_PATH=" + filepath.Join(libDir, "gstreamer-1.0"),
		"GST_PLUGIN_SYSTEM_PATH=",
		"GST_PLUGIN_SCANNER=" + filepath.Join(root, "libexec", "gstreamer-1.0", "gst-plugin-scanner"),
	}
}
