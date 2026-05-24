package audiobridge

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtp"
)

const (
	defaultSpeexInPort   = 51060
	defaultPCMDownPort   = 51062
	defaultOpusOutPort   = 51064
	defaultOpusInPort    = 51066
	defaultPCMUpPort     = 51068
	defaultSpeexOutPort  = 51070
	defaultOpusPayloadPT = 111
	defaultBackOpusPT    = 112

	pipelineRestartDelay       = time.Second
	pipelineMaxRestartAttempts = 6
)

type pipelineSpec struct {
	name   string
	bin    string
	args   []string
	bundle bool
}

type managedProcess struct {
	spec pipelineSpec
	cmd  *exec.Cmd
}

type Ports struct {
	SpeexIn  int
	PCMDown  int
	OpusOut  int
	OpusIn   int
	PCMUp    int
	SpeexOut int
}

type Config struct {
	Enabled                  bool
	BundleRoot               string
	Ports                    Ports
	OpusPayloadType          uint8
	BackchannelOpusPayloadPT uint8
	OpusBitrate              int
	OpusComplexity           int
	OpusFrameMs              int
	OpusDTX                  bool
	OpusFEC                  bool
}

func DefaultConfig(dataDir string) Config {
	return Config{
		Enabled:                  true,
		BundleRoot:               filepath.Join(strings.TrimSpace(dataDir), "gst"),
		Ports:                    DefaultPorts(),
		OpusPayloadType:          defaultOpusPayloadPT,
		BackchannelOpusPayloadPT: defaultBackOpusPT,
		OpusBitrate:              24000,
		OpusComplexity:           5,
		OpusFrameMs:              20,
		OpusDTX:                  false,
		OpusFEC:                  false,
	}
}

func DefaultPorts() Ports {
	return Ports{
		SpeexIn:  defaultSpeexInPort,
		PCMDown:  defaultPCMDownPort,
		OpusOut:  defaultOpusOutPort,
		OpusIn:   defaultOpusInPort,
		PCMUp:    defaultPCMUpPort,
		SpeexOut: defaultSpeexOutPort,
	}
}

type Service struct {
	cfg    Config
	logger *log.Logger

	mu       sync.Mutex
	refCount int
	running  bool
	cancel   context.CancelFunc
	procs    []*managedProcess
	wg       sync.WaitGroup

	speexInConn *net.UDPConn
	opusInConn  *net.UDPConn
}

func New(cfg Config, logger *log.Logger) *Service {
	if logger == nil {
		logger = log.Default()
	}
	return &Service{
		cfg:    cfg,
		logger: logger,
	}
}

func (s *Service) Enabled() bool {
	return s != nil && s.cfg.Enabled
}

func (s *Service) OpusPayloadType() uint8 {
	if s == nil || s.cfg.OpusPayloadType == 0 {
		return defaultOpusPayloadPT
	}
	return s.cfg.OpusPayloadType
}

func (s *Service) BackchannelOpusPayloadType() uint8 {
	if s == nil || s.cfg.BackchannelOpusPayloadPT == 0 {
		return defaultBackOpusPT
	}
	return s.cfg.BackchannelOpusPayloadPT
}

func (s *Service) Ports() Ports {
	if s == nil {
		return DefaultPorts()
	}
	return s.cfg.Ports
}

func (s *Service) Start(ctx context.Context) error {
	if s == nil || !s.cfg.Enabled {
		return nil
	}

	s.mu.Lock()
	if s.running {
		s.refCount++
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	runCtx, cancel := context.WithCancel(context.Background())
	procs, err := s.startPipelines(runCtx)
	if err != nil {
		cancel()
		return err
	}

	s.mu.Lock()
	s.cancel = cancel
	s.procs = procs
	s.refCount = 1
	s.running = true
	s.mu.Unlock()

	go func() {
		<-ctx.Done()
		_ = s.Stop(context.Background())
	}()

	s.logger.Printf("audio bridge started")
	return nil
}

func (s *Service) Stop(_ context.Context) error {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return nil
	}
	if s.refCount > 1 {
		s.refCount--
		s.mu.Unlock()
		return nil
	}

	cancel := s.cancel
	procs := append([]*managedProcess(nil), s.procs...)
	s.cancel = nil
	s.procs = nil
	s.refCount = 0
	s.running = false
	s.closeInputsLocked()
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	for _, cmd := range procs {
		if cmd != nil && cmd.cmd != nil && cmd.cmd.Process != nil {
			_ = cmd.cmd.Process.Kill()
		}
	}
	s.wg.Wait()
	s.logger.Printf("audio bridge stopped")
	return nil
}

func (s *Service) WriteIntercomSpeex(pkt *rtp.Packet) error {
	if s == nil || pkt == nil || !s.cfg.Enabled {
		return nil
	}
	raw, err := pkt.Marshal()
	if err != nil {
		return err
	}
	conn, err := s.speexInputConnection()
	if err != nil {
		return err
	}
	_, err = conn.Write(raw)
	return err
}

func (s *Service) WriteBackchannelOpus(pkt *rtp.Packet) error {
	if s == nil || pkt == nil || !s.cfg.Enabled {
		return nil
	}
	raw, err := pkt.Marshal()
	if err != nil {
		return err
	}
	conn, err := s.opusInputConnection()
	if err != nil {
		return err
	}
	_, err = conn.Write(raw)
	return err
}

func (s *Service) speexInputConnection() (*net.UDPConn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.speexInConn != nil {
		return s.speexInConn, nil
	}
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: s.cfg.Ports.SpeexIn}
	conn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		return nil, fmt.Errorf("dial bridge speex input: %w", err)
	}
	s.speexInConn = conn
	return conn, nil
}

func (s *Service) opusInputConnection() (*net.UDPConn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.opusInConn != nil {
		return s.opusInConn, nil
	}
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: s.cfg.Ports.OpusIn}
	conn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		return nil, fmt.Errorf("dial bridge opus input: %w", err)
	}
	s.opusInConn = conn
	return conn, nil
}

func (s *Service) startPipelines(ctx context.Context) ([]*managedProcess, error) {
	cmdSpecs := []pipelineSpec{
		{
			name: "downlink_speex_to_l16",
			bin:  "/usr/bin/gst-launch-1.0",
			args: []string{
				"-q",
				"udpsrc", fmt.Sprintf("port=%d", s.cfg.Ports.SpeexIn),
				"caps=application/x-rtp,media=audio,encoding-name=SPEEX,clock-rate=8000,payload=110",
				"!",
				"rtpspeexdepay", "!", "speexdec", "!", "audioconvert", "!", "audioresample", "!",
				"audio/x-raw,format=S16BE,rate=8000,channels=1",
				"!",
				"rtpL16pay", "pt=96",
				"!",
				"udpsink", "host=127.0.0.1", fmt.Sprintf("port=%d", s.cfg.Ports.PCMDown),
			},
		},
		{
			name:   "downlink_l16_to_opus",
			bin:    filepath.Join(s.cfg.BundleRoot, "bin", "gst-launch-1.0"),
			bundle: true,
			args: []string{
				"-q",
				"udpsrc", fmt.Sprintf("port=%d", s.cfg.Ports.PCMDown),
				"caps=application/x-rtp,media=audio,encoding-name=L16,clock-rate=8000,channels=1,encoding-params=1,payload=96",
				"!",
				"rtpL16depay", "!", "audioconvert", "!", "audioresample", "!",
				"audio/x-raw,format=S16LE,rate=48000,channels=1",
				"!",
				"opusenc",
				fmt.Sprintf("bitrate=%d", s.cfg.OpusBitrate),
				fmt.Sprintf("complexity=%d", s.cfg.OpusComplexity),
				fmt.Sprintf("frame-size=%d", s.cfg.OpusFrameMs),
				fmt.Sprintf("dtx=%t", s.cfg.OpusDTX),
				fmt.Sprintf("inband-fec=%t", s.cfg.OpusFEC),
				"audio-type=voice",
				"!",
				"rtpopuspay", fmt.Sprintf("pt=%d", s.cfg.OpusPayloadType),
				"!",
				"udpsink", "host=127.0.0.1", fmt.Sprintf("port=%d", s.cfg.Ports.OpusOut),
			},
		},
		{
			name:   "uplink_opus_to_l16",
			bin:    filepath.Join(s.cfg.BundleRoot, "bin", "gst-launch-1.0"),
			bundle: true,
			args: []string{
				"-q",
				"udpsrc", fmt.Sprintf("port=%d", s.cfg.Ports.OpusIn),
				fmt.Sprintf("caps=application/x-rtp,media=audio,encoding-name=OPUS,clock-rate=48000,payload=%d", s.cfg.BackchannelOpusPayloadPT),
				"!",
				"rtpopusdepay", "!", "opusdec", "!", "audioconvert", "!", "audioresample", "!",
				"audio/x-raw,format=S16BE,rate=8000,channels=1",
				"!",
				"rtpL16pay", "pt=96",
				"!",
				"udpsink", "host=127.0.0.1", fmt.Sprintf("port=%d", s.cfg.Ports.PCMUp),
			},
		},
		{
			name: "uplink_l16_to_speex",
			bin:  "/usr/bin/gst-launch-1.0",
			args: []string{
				"-q",
				"udpsrc", fmt.Sprintf("port=%d", s.cfg.Ports.PCMUp),
				"caps=application/x-rtp,media=audio,encoding-name=L16,clock-rate=8000,channels=1,encoding-params=1,payload=96",
				"!",
				"rtpL16depay", "!", "audioconvert", "!", "audioresample", "!",
				"audio/x-raw,rate=8000,channels=1",
				"!",
				"speexenc",
				"!",
				"rtpspeexpay", "pt=97",
				"!",
				"udpsink", "host=127.0.0.1", fmt.Sprintf("port=%d", s.cfg.Ports.SpeexOut),
			},
		},
	}

	var out []*managedProcess
	for _, spec := range cmdSpecs {
		cmd := exec.CommandContext(ctx, spec.bin, spec.args...)
		if spec.bundle {
			cmd.Env = append([]string{}, bundledGSTEnv(s.cfg.BundleRoot)...)
		}
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Start(); err != nil {
			for _, started := range out {
				if started != nil && started.cmd != nil && started.cmd.Process != nil {
					_ = started.cmd.Process.Kill()
				}
			}
			return nil, fmt.Errorf("start audio bridge pipeline %s: %w", spec.name, err)
		}
		out = append(out, &managedProcess{spec: spec, cmd: cmd})
	}
	for _, proc := range out {
		if proc == nil {
			continue
		}
		s.wg.Add(1)
		go s.supervisePipeline(ctx, proc)
	}
	return out, nil
}

func (s *Service) supervisePipeline(ctx context.Context, proc *managedProcess) {
	defer s.wg.Done()
	if proc == nil {
		return
	}
	restarts := 0
	for {
		cmd := proc.cmd
		if cmd == nil {
			return
		}
		err := cmd.Wait()
		if ctx.Err() != nil {
			return
		}
		restarts++
		if err != nil {
			s.logger.Printf("audio bridge pipeline exited name=%s err=%v restart=%d/%d", proc.spec.name, err, restarts, pipelineMaxRestartAttempts)
		} else {
			s.logger.Printf("audio bridge pipeline exited name=%s restart=%d/%d", proc.spec.name, restarts, pipelineMaxRestartAttempts)
		}
		if restarts > pipelineMaxRestartAttempts {
			s.logger.Printf("audio bridge pipeline disabled after restart budget name=%s", proc.spec.name)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pipelineRestartDelay):
		}

		next := exec.CommandContext(ctx, proc.spec.bin, proc.spec.args...)
		if proc.spec.bundle {
			next.Env = append([]string{}, bundledGSTEnv(s.cfg.BundleRoot)...)
		}
		next.Stdout = io.Discard
		next.Stderr = io.Discard
		if err := next.Start(); err != nil {
			s.logger.Printf("audio bridge pipeline restart failed name=%s err=%v", proc.spec.name, err)
			continue
		}
		s.mu.Lock()
		if !s.running {
			s.mu.Unlock()
			if next.Process != nil {
				_ = next.Process.Kill()
			}
			_ = next.Wait()
			return
		}
		proc.cmd = next
		s.mu.Unlock()
	}
}

func bundledGSTEnv(bundleRoot string) []string {
	root := strings.TrimSpace(bundleRoot)
	libDir := filepath.Join(root, "lib")
	pluginDir := filepath.Join(libDir, "gstreamer-1.0")
	scanner := filepath.Join(root, "libexec", "gstreamer-1.0", "gst-plugin-scanner")
	return []string{
		"PATH=/usr/bin:/bin",
		"LD_LIBRARY_PATH=" + libDir,
		"GST_PLUGIN_PATH=" + pluginDir,
		"GST_PLUGIN_SYSTEM_PATH=",
		"GST_PLUGIN_SCANNER=" + scanner,
	}
}

func (s *Service) closeInputsLocked() {
	if s.speexInConn != nil {
		_ = s.speexInConn.Close()
		s.speexInConn = nil
	}
	if s.opusInConn != nil {
		_ = s.opusInConn.Close()
		s.opusInConn = nil
	}
}
