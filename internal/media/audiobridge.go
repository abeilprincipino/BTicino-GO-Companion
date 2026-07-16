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
)

type AudioPipeline interface {
	WriteIntercomSpeex(*rtp.Packet) error
	WriteBackchannelOpus(*rtp.Packet) error
	ReadOpusOut() <-chan *rtp.Packet
	ReadSpeexOut() <-chan *rtp.Packet
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
	pipeline    AudioPipeline
	cancel      context.CancelFunc
	done        chan struct{}
}

func NewAudioBridge(
	gstreamer GStreamerAudio,
	opusOutput func(*rtp.Packet),
	backchannel Backchannel,
) *AudioBridge {
	return &AudioBridge{
		gstreamer:   gstreamer,
		opusOutput:  opusOutput,
		backchannel: backchannel,
	}
}

func (b *AudioBridge) Start(ctx context.Context) error {
	if b == nil || b.gstreamer == nil || b.opusOutput == nil {
		return ErrAudioBridgeUnavailable
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.pipeline != nil {
		return ErrAudioBridgeStarted
	}

	pipeline, err := b.gstreamer.StartAudioBridge(ctx)
	if err != nil {
		return err
	}

	if pipeline == nil {
		return ErrAudioBridgeUnavailable
	}

	runCtx, cancel := context.WithCancel(context.Background())
	b.pipeline = pipeline
	b.cancel = cancel

	b.done = make(chan struct{})
	go b.forward(runCtx, pipeline, b.done)

	return nil
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
	if b == nil {
		return nil
	}

	b.mu.Lock()

	pipeline := b.pipeline
	if pipeline == nil {
		b.mu.Unlock()
		return nil
	}

	b.pipeline = nil
	cancel := b.cancel
	done := b.done
	b.cancel = nil
	b.done = nil
	b.mu.Unlock()

	cancel()

	err := pipeline.Close()

	<-done

	return err
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

// GStreamerAudioBridge is the V2-compatible process implementation of GStreamerAudio.
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
	p := &gstreamerAudioPipeline{logger: g.logger, opusConn: opusConn, speexConn: speexConn, opusOut: make(chan *rtp.Packet, 32), speexOut: make(chan *rtp.Packet, 32)}
	bundle := filepath.Join(g.bundleRoot, "bin", "gst-launch-1.0")
	p.commands = []*exec.Cmd{
		exec.CommandContext(ctx, "/usr/bin/gst-launch-1.0", "-q", "udpsrc", "port=51060", "caps=application/x-rtp,media=audio,encoding-name=SPEEX,clock-rate=8000,payload=110", "!", "rtpspeexdepay", "!", "speexdec", "!", "audioconvert", "!", "audioresample", "!", "audio/x-raw,format=S16BE,rate=8000,channels=1", "!", "rtpL16pay", "pt=96", "!", "udpsink", "host=127.0.0.1", "port=51062"),
		exec.CommandContext(ctx, bundle, "-q", "udpsrc", "port=51062", "caps=application/x-rtp,media=audio,encoding-name=L16,clock-rate=8000,channels=1,encoding-params=1,payload=96", "!", "rtpL16depay", "!", "audioconvert", "!", "audioresample", "!", "audio/x-raw,format=S16LE,rate=48000,channels=1", "!", "opusenc", "bitrate=24000", "complexity=5", "frame-size=20", "dtx=false", "inband-fec=false", "audio-type=voice", "!", "rtpopuspay", "pt=111", "!", "udpsink", "host=127.0.0.1", "port=51064"),
		exec.CommandContext(ctx, bundle, "-q", "udpsrc", "port=51066", "caps=application/x-rtp,media=audio,encoding-name=OPUS,clock-rate=48000,payload=112", "!", "rtpopusdepay", "!", "opusdec", "!", "audioconvert", "!", "audioresample", "!", "audio/x-raw,format=S16BE,rate=8000,channels=1", "!", "rtpL16pay", "pt=96", "!", "udpsink", "host=127.0.0.1", "port=51068"),
		exec.CommandContext(ctx, "/usr/bin/gst-launch-1.0", "-q", "udpsrc", "port=51068", "caps=application/x-rtp,media=audio,encoding-name=L16,clock-rate=8000,channels=1,encoding-params=1,payload=96", "!", "rtpL16depay", "!", "audioconvert", "!", "audioresample", "!", "audio/x-raw,rate=8000,channels=1", "!", "speexenc", "!", "rtpspeexpay", "pt=97", "!", "udpsink", "host=127.0.0.1", "port=51070"),
	}
	for index, command := range p.commands {
		if index == 1 || index == 2 {
			command.Env = bundledGSTEnv(g.bundleRoot)
		}
		if err := command.Start(); err != nil {
			_ = p.Close()
			return nil, fmt.Errorf("start audio bridge pipeline: %w", err)
		}
		go p.wait(command)
	}
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
	closed                               bool
}

func (p *gstreamerAudioPipeline) wait(command *exec.Cmd) {
	err := command.Wait()
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if !closed {
		p.logger.Error("audio bridge pipeline exited", "error", err)
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
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	for _, conn := range []*net.UDPConn{p.opusConn, p.speexConn, p.speexIn, p.opusIn} {
		if conn != nil {
			_ = conn.Close()
		}
	}
	for _, command := range p.commands {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
	}
	p.mu.Unlock()
	return nil
}
func (p *gstreamerAudioPipeline) read(conn *net.UDPConn, payloadType uint8, output chan<- *rtp.Packet) {
	defer close(output)
	buffer := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		packet := &rtp.Packet{}
		if packet.Unmarshal(buffer[:n]) == nil && packet.PayloadType == payloadType {
			output <- packet
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
