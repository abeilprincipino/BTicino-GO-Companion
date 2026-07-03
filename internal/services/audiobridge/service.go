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

	// pipelineStderrTailBytes bounds how much of a gst pipeline's stderr we
	// retain per process so a failure reason survives into the exit log without
	// letting a chatty pipeline grow memory without bound.
	pipelineStderrTailBytes = 2048
)

type pipelineSpec struct {
	name   string
	bin    string
	args   []string
	bundle bool
}

type managedProcess struct {
	spec   pipelineSpec
	cmd    *exec.Cmd
	stderr *tailWriter
}

// tailWriter is an io.Writer that retains only the last cap bytes written to
// it. It is used to capture the tail of a gst pipeline's stderr so we can log
// the failure reason without letting a chatty pipeline balloon memory. It is
// safe for concurrent use: the child process writes from an os/exec goroutine
// while the supervisor reads the tail after Wait returns.
type tailWriter struct {
	mu  sync.Mutex
	cap int
	buf []byte
}

func newTailWriter(cap int) *tailWriter {
	if cap < 0 {
		cap = 0
	}
	return &tailWriter{cap: cap, buf: make([]byte, 0, cap)}
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	total := len(p)
	if w.cap == 0 {
		return total, nil
	}
	if len(p) >= w.cap {
		// The incoming chunk alone overflows the cap: keep only its tail.
		w.buf = append(w.buf[:0], p[len(p)-w.cap:]...)
		return total, nil
	}
	if len(w.buf)+len(p) > w.cap {
		// Drop the oldest bytes to make room for the new chunk.
		drop := len(w.buf) + len(p) - w.cap
		w.buf = w.buf[drop:]
	}
	w.buf = append(w.buf, p...)
	return total, nil
}

func (w *tailWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
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
	starting bool
	startDone chan struct{}
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
	for s.starting {
		done := s.startDone
		s.mu.Unlock()
		if done != nil {
			<-done
		}
		s.mu.Lock()
	}
	if s.running {
		s.mu.Unlock()
		return nil
	}
	s.starting = true
	s.startDone = make(chan struct{})
	done := s.startDone
	s.mu.Unlock()

	runCtx, cancel := context.WithCancel(context.Background())
	procs, err := s.startPipelines(runCtx)
	s.mu.Lock()
	s.starting = false
	if done != nil {
		close(done)
	}
	s.startDone = nil
	if err == nil {
		s.cancel = cancel
		s.procs = procs
		s.running = true
	}
	s.mu.Unlock()
	if err != nil {
		cancel()
		return err
	}

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
	for s.starting {
		done := s.startDone
		s.mu.Unlock()
		if done != nil {
			<-done
		}
		s.mu.Lock()
	}
	if !s.running {
		s.mu.Unlock()
		return nil
	}

	cancel := s.cancel
	procs := append([]*managedProcess(nil), s.procs...)
	s.cancel = nil
	s.procs = nil
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
		stderr := newTailWriter(pipelineStderrTailBytes)
		cmd.Stdout = io.Discard
		cmd.Stderr = stderr
		// Log the full command once per pipeline at first start; args are long,
		// so keep them on their own line and out of the recurring exit logs.
		s.logger.Printf("audio bridge pipeline starting name=%s bin=%s", spec.name, spec.bin)
		s.logger.Printf("audio bridge pipeline command name=%s args=%s", spec.name, strings.Join(spec.args, " "))
		if err := cmd.Start(); err != nil {
			for _, started := range out {
				if started != nil && started.cmd != nil && started.cmd.Process != nil {
					_ = started.cmd.Process.Kill()
				}
			}
			return nil, fmt.Errorf("start audio bridge pipeline %s: %w", spec.name, err)
		}
		out = append(out, &managedProcess{spec: spec, cmd: cmd, stderr: stderr})
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
		var tail string
		if proc.stderr != nil {
			tail = proc.stderr.String()
		}
		if tail != "" {
			s.logger.Printf("audio bridge pipeline exited name=%s err=%v restart=%d/%d stderr=%q", proc.spec.name, err, restarts, pipelineMaxRestartAttempts, tail)
		} else {
			s.logger.Printf("audio bridge pipeline exited name=%s err=%v restart=%d/%d", proc.spec.name, err, restarts, pipelineMaxRestartAttempts)
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
		// Fresh stderr capture per process — do not share the buffer across
		// restarts so each exit log reflects only that run's output.
		nextStderr := newTailWriter(pipelineStderrTailBytes)
		next.Stdout = io.Discard
		next.Stderr = nextStderr
		s.logger.Printf("audio bridge pipeline starting name=%s bin=%s", proc.spec.name, proc.spec.bin)
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
		proc.stderr = nextStderr
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
