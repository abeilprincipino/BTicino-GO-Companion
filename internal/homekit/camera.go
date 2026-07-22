package homekit

import (
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/media"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/brutella/hap/service"
	"github.com/brutella/hap/tlv8"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
)

const (
	cameraVideoPayloadType = 99
	cameraAudioPayloadType = 110
	cameraSRTPKeyLength    = 16
	cameraSRTPSaltLength   = 14
	cameraStartupTimeout   = 15 * time.Second
	cameraRTCPInterval     = 5 * time.Second

	cameraCryptoAESCM128HMACSHA180 = 0
	cameraStreamingAvailable       = 0
	cameraStreamingInUse           = 1
	cameraCommandEnd               = 0
	cameraCommandStart             = 1
	cameraCommandSuspend           = 2
	cameraCommandResume            = 3
	cameraCommandReconfigure       = 4
)

type cameraSessionManager struct {
	coordinator *media.StreamCoordinator
	entrypoint  config.Entrypoint
	service     *service.CameraRTPStreamManagement
	logger      *slog.Logger

	mu     sync.Mutex
	active *cameraSession
}

type cameraSession struct {
	id  string
	ctx context.Context

	remoteAddress cameraAddress
	videoSSRC     uint32
	audioSSRC     uint32
	videoConn     *net.UDPConn
	audioConn     *net.UDPConn
	videoSRTP     *srtp.Context
	audioSRTP     *srtp.Context
	videoTarget   *net.UDPAddr
	audioTarget   *net.UDPAddr

	lease      *media.StreamLease
	cancel     context.CancelFunc
	started    bool
	closed     atomic.Bool
	rtcpCancel context.CancelFunc

	videoPackets atomic.Uint32
	videoOctets  atomic.Uint32
	videoTime    atomic.Uint32
	audioPackets atomic.Uint32
	audioOctets  atomic.Uint32
	audioTime    atomic.Uint32
}

// HomeKit camera stream negotiation uses these TLV8 structures. They mirror
// the HAP camera characteristics exposed by Bruttella.
type cameraSetupEndpointsRequest struct {
	SessionID   string                `tlv8:"1"`
	Address     cameraAddress         `tlv8:"3"`
	VideoCrypto cameraSRTPCryptoSuite `tlv8:"4"`
	AudioCrypto cameraSRTPCryptoSuite `tlv8:"5"`
}

type cameraSetupEndpointsResponse struct {
	SessionID   string                `tlv8:"1"`
	Status      byte                  `tlv8:"2"`
	Address     cameraAddress         `tlv8:"3"`
	VideoCrypto cameraSRTPCryptoSuite `tlv8:"4"`
	AudioCrypto cameraSRTPCryptoSuite `tlv8:"5"`
	VideoSSRC   uint32                `tlv8:"6"`
	AudioSSRC   uint32                `tlv8:"7"`
}

type cameraSelectedStreamConfiguration struct {
	Control    cameraSessionControl     `tlv8:"1"`
	VideoCodec cameraVideoConfiguration `tlv8:"2"`
	AudioCodec cameraAudioConfiguration `tlv8:"3"`
}

type cameraSessionControl struct {
	SessionID string `tlv8:"1"`
	Command   byte   `tlv8:"2"`
}

type cameraAddress struct {
	IPVersion    byte   `tlv8:"1"`
	IPAddr       string `tlv8:"2"`
	VideoRTPPort uint16 `tlv8:"3"`
	AudioRTPPort uint16 `tlv8:"4"`
}

type cameraSRTPCryptoSuite struct {
	CryptoSuite byte   `tlv8:"1"`
	MasterKey   string `tlv8:"2"`
	MasterSalt  string `tlv8:"3"`
}

type cameraRTPParameters struct {
	PayloadType uint8 `tlv8:"1"`
}

type cameraVideoConfiguration struct {
	RTPParameters []cameraRTPParameters `tlv8:"4"`
}

type cameraAudioConfiguration struct {
	RTPParameters []cameraRTPParameters `tlv8:"3"`
}

type cameraStreamingStatus struct {
	Status byte `tlv8:"1"`
}

type cameraSupportedVideoConfiguration struct {
	Codecs []cameraVideoCodecConfiguration `tlv8:"1"`
}

type cameraVideoCodecConfiguration struct {
	CodecType   byte                         `tlv8:"1"`
	CodecParams []cameraVideoCodecParameters `tlv8:"2"`
	VideoAttrs  []cameraVideoAttributes      `tlv8:"3"`
}

type cameraVideoCodecParameters struct {
	ProfileID         byte `tlv8:"1"`
	Level             byte `tlv8:"2"`
	PacketizationMode byte `tlv8:"3"`
}

type cameraVideoAttributes struct {
	Width     uint16 `tlv8:"1"`
	Height    uint16 `tlv8:"2"`
	Framerate uint8  `tlv8:"3"`
}

type cameraSupportedAudioConfiguration struct {
	Codecs              []cameraAudioCodecConfiguration `tlv8:"1"`
	ComfortNoiseSupport byte                            `tlv8:"2"`
}

type cameraAudioCodecConfiguration struct {
	CodecType    byte                         `tlv8:"1"`
	CodecParams  []cameraAudioCodecParameters `tlv8:"2"`
	ComfortNoise []byte                       `tlv8:"4"`
}

type cameraAudioCodecParameters struct {
	Channels    uint8 `tlv8:"1"`
	BitrateMode byte  `tlv8:"2"`
	SampleRate  byte  `tlv8:"3"`
}

type cameraSupportedRTPConfiguration struct {
	SRTPCryptoType []byte `tlv8:"2"`
}

func newCameraSessionManager(coordinator *media.StreamCoordinator, entrypoint config.Entrypoint, cameraService *service.CameraRTPStreamManagement, logger *slog.Logger) *cameraSessionManager {
	if logger == nil {
		logger = slog.Default()
	}

	m := &cameraSessionManager{
		coordinator: coordinator,
		entrypoint:  entrypoint,
		service:     cameraService,
		logger:      logger,
	}
	m.configureService()

	return m
}

func (m *cameraSessionManager) configureService() {
	m.service.SupportedVideoStreamConfiguration.SetValue(mustCameraTLV(cameraSupportedVideoConfiguration{
		Codecs: []cameraVideoCodecConfiguration{{
			CodecType: 0, // H.264
			CodecParams: []cameraVideoCodecParameters{{
				ProfileID:         1, // Main profile
				Level:             0, // HAP level 3.1
				PacketizationMode: 0,
			}},
			VideoAttrs: []cameraVideoAttributes{
				{Width: 1920, Height: 1080, Framerate: 30},
				{Width: 1280, Height: 720, Framerate: 30},
				{Width: 320, Height: 240, Framerate: 15},
			},
		}},
	}))
	m.service.SupportedAudioStreamConfiguration.SetValue(mustCameraTLV(cameraSupportedAudioConfiguration{
		Codecs: []cameraAudioCodecConfiguration{{
			CodecType: 3, // Opus
			CodecParams: []cameraAudioCodecParameters{{
				Channels:    1,
				BitrateMode: 0,
				SampleRate:  1, // HAP 16 kHz Opus mode
			}},
		}},
	}))
	m.service.SupportedRTPConfiguration.SetValue(mustCameraTLV(cameraSupportedRTPConfiguration{
		SRTPCryptoType: []byte{cameraCryptoAESCM128HMACSHA180},
	}))
	m.setStreamingStatus(cameraStreamingAvailable)

	// Bruttella's camera implementation updates Setup Endpoints to the response
	// after HomeKit writes its controller parameters.
	m.service.SetupEndpoints.OnValueUpdate(func(value, _ []byte, request *http.Request) {
		if request == nil {
			return
		}

		m.logger.Info("homekit camera endpoints requested", "entrypoint_id", m.entrypoint.ID)

		response, err := m.setupEndpoints(value, request)
		if err != nil {
			m.logger.Error("configure homekit camera endpoints", "entrypoint_id", m.entrypoint.ID, "error", err)
			return
		}

		m.service.SetupEndpoints.SetValue(response)
	})
	m.service.SelectedRTPStreamConfiguration.OnSetRemoteValue(m.handleSelectedStreamConfiguration)
}

func (m *cameraSessionManager) setupEndpoints(data []byte, httpRequest *http.Request) ([]byte, error) {
	var request cameraSetupEndpointsRequest
	if err := tlv8.Unmarshal(data, &request); err != nil {
		return nil, fmt.Errorf("decode setup endpoints: %w", err)
	}

	if request.SessionID == "" || net.ParseIP(request.Address.IPAddr) == nil || request.Address.VideoRTPPort == 0 || request.Address.AudioRTPPort == 0 {
		return nil, errors.New("invalid setup endpoints request")
	}

	if request.VideoCrypto.CryptoSuite != cameraCryptoAESCM128HMACSHA180 || request.AudioCrypto.CryptoSuite != cameraCryptoAESCM128HMACSHA180 {
		return nil, errors.New("unsupported SRTP crypto suite")
	}

	localIP, err := cameraLocalIP(httpRequest, request.Address.IPVersion)
	if err != nil {
		return nil, fmt.Errorf("detect local address: %w", err)
	}

	videoConn, videoPort, err := cameraUDPPort()
	if err != nil {
		return nil, fmt.Errorf("allocate video port: %w", err)
	}

	audioConn, audioPort, err := cameraUDPPort()
	if err != nil {
		_ = videoConn.Close()
		return nil, fmt.Errorf("allocate audio port: %w", err)
	}

	videoResponseCrypto, err := cameraCrypto()
	if err != nil {
		_ = videoConn.Close()
		_ = audioConn.Close()

		return nil, err
	}

	audioResponseCrypto, err := cameraCrypto()
	if err != nil {
		_ = videoConn.Close()
		_ = audioConn.Close()

		return nil, err
	}

	videoSRTP, err := cameraSRTPContext(videoResponseCrypto)
	if err != nil {
		_ = videoConn.Close()
		_ = audioConn.Close()

		return nil, fmt.Errorf("create video SRTP context: %w", err)
	}

	audioSRTP, err := cameraSRTPContext(audioResponseCrypto)
	if err != nil {
		_ = videoConn.Close()
		_ = audioConn.Close()

		return nil, fmt.Errorf("create audio SRTP context: %w", err)
	}

	videoSSRC, err := cameraRandomUint32()
	if err != nil {
		_ = videoConn.Close()
		_ = audioConn.Close()

		return nil, err
	}

	audioSSRC, err := cameraRandomUint32()
	if err != nil {
		_ = videoConn.Close()
		_ = audioConn.Close()

		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	session := &cameraSession{
		id:            request.SessionID,
		ctx:           ctx,
		remoteAddress: request.Address,
		videoSSRC:     videoSSRC,
		audioSSRC:     audioSSRC,
		videoConn:     videoConn,
		audioConn:     audioConn,
		videoSRTP:     videoSRTP,
		audioSRTP:     audioSRTP,
		videoTarget:   &net.UDPAddr{IP: net.ParseIP(request.Address.IPAddr), Port: int(request.Address.VideoRTPPort)},
		audioTarget:   &net.UDPAddr{IP: net.ParseIP(request.Address.IPAddr), Port: int(request.Address.AudioRTPPort)},
		cancel:        cancel,
	}

	m.mu.Lock()
	previous := m.active
	m.active = session
	m.mu.Unlock()
	m.closeSession(previous)

	response, err := tlv8.Marshal(cameraSetupEndpointsResponse{
		SessionID: request.SessionID,
		Status:    0,
		Address: cameraAddress{
			IPVersion:    0,
			IPAddr:       localIP.String(),
			VideoRTPPort: videoPort,
			AudioRTPPort: audioPort,
		},
		VideoCrypto: videoResponseCrypto,
		AudioCrypto: audioResponseCrypto,
		VideoSSRC:   session.videoSSRC,
		AudioSSRC:   session.audioSSRC,
	})
	if err != nil {
		m.closeSession(session)
		return nil, fmt.Errorf("encode setup endpoints response: %w", err)
	}

	return response, nil
}

func (m *cameraSessionManager) handleSelectedStreamConfiguration(data []byte) error {
	var request cameraSelectedStreamConfiguration
	if err := tlv8.Unmarshal(data, &request); err != nil {
		return fmt.Errorf("decode selected stream configuration: %w", err)
	}

	m.mu.Lock()
	session := m.active
	m.mu.Unlock()

	if session == nil || session.id != request.Control.SessionID {
		return errors.New("homekit camera session is unavailable")
	}

	m.logger.Info("homekit camera stream command", "entrypoint_id", m.entrypoint.ID, "command", request.Control.Command)

	switch request.Control.Command {
	case cameraCommandStart:
		return m.start(session, request)
	case cameraCommandEnd, cameraCommandSuspend:
		m.clearSession(session)
		return nil
	case cameraCommandResume, cameraCommandReconfigure:
		return nil
	default:
		return fmt.Errorf("unsupported camera stream command %d", request.Control.Command)
	}
}

func (m *cameraSessionManager) start(session *cameraSession, request cameraSelectedStreamConfiguration) error {
	m.mu.Lock()
	if m.active != session || session.closed.Load() {
		m.mu.Unlock()
		return errors.New("homekit camera session is unavailable")
	}

	if session.started {
		m.mu.Unlock()
		return nil
	}

	session.started = true
	m.mu.Unlock()

	startupCtx, cancel := context.WithTimeout(context.Background(), cameraStartupTimeout)
	lease, err := m.coordinator.AcquireWithStartup(sessionContext(session), startupCtx, m.entrypoint, media.SourceEvents{
		VideoRTP:  func(packet *rtp.Packet) { m.forwardVideo(session, request, packet) },
		AudioRTP:  func(packet *rtp.Packet) { m.forwardAudio(session, request, packet) },
		RemoteBYE: func() { m.clearSession(session) },
		Failed:    func(error) { m.clearSession(session) },
	})

	cancel()

	if err != nil {
		m.clearSession(session)
		return fmt.Errorf("acquire intercom stream: %w", err)
	}

	m.mu.Lock()
	if m.active != session || session.closed.Load() {
		m.mu.Unlock()
		m.coordinator.Release(lease)

		return errors.New("homekit camera session closed during startup")
	}

	session.lease = lease
	rtcpCtx, rtcpCancel := context.WithCancel(sessionContext(session))
	session.rtcpCancel = rtcpCancel
	m.mu.Unlock()

	m.setStreamingStatus(cameraStreamingInUse)
	go m.sendRTCP(rtcpCtx, session)

	m.logger.Info("homekit camera stream started", "entrypoint_id", m.entrypoint.ID, "session_id", session.id)

	return nil
}

func (m *cameraSessionManager) clearSession(session *cameraSession) {
	m.mu.Lock()
	if m.active == session {
		m.active = nil
	}
	m.mu.Unlock()
	m.closeSession(session)
}

func (m *cameraSessionManager) closeSession(session *cameraSession) {
	if session == nil || !session.closed.CompareAndSwap(false, true) {
		return
	}

	if session.rtcpCancel != nil {
		session.rtcpCancel()
	}

	if session.cancel != nil {
		session.cancel()
	}

	if session.lease != nil {
		m.coordinator.Release(session.lease)
	}

	if session.videoConn != nil {
		_ = session.videoConn.Close()
	}

	if session.audioConn != nil {
		_ = session.audioConn.Close()
	}

	m.setStreamingStatus(cameraStreamingAvailable)
	m.logger.Info("homekit camera stream stopped", "entrypoint_id", m.entrypoint.ID, "session_id", session.id)
}

func (m *cameraSessionManager) forwardVideo(session *cameraSession, request cameraSelectedStreamConfiguration, packet *rtp.Packet) {
	payloadType := uint8(cameraVideoPayloadType)
	if len(request.VideoCodec.RTPParameters) > 0 && request.VideoCodec.RTPParameters[0].PayloadType != 0 {
		payloadType = request.VideoCodec.RTPParameters[0].PayloadType
	}

	m.forward(session, packet, payloadType, session.videoSSRC, session.videoConn, session.videoSRTP, session.videoTarget, &session.videoPackets, &session.videoOctets, &session.videoTime, "video")
}

func (m *cameraSessionManager) forwardAudio(session *cameraSession, request cameraSelectedStreamConfiguration, packet *rtp.Packet) {
	payloadType := uint8(cameraAudioPayloadType)
	if len(request.AudioCodec.RTPParameters) > 0 && request.AudioCodec.RTPParameters[0].PayloadType != 0 {
		payloadType = request.AudioCodec.RTPParameters[0].PayloadType
	}

	m.forward(session, packet, payloadType, session.audioSSRC, session.audioConn, session.audioSRTP, session.audioTarget, &session.audioPackets, &session.audioOctets, &session.audioTime, "audio")
}

func (m *cameraSessionManager) forward(session *cameraSession, packet *rtp.Packet, payloadType uint8, ssrc uint32, conn *net.UDPConn, crypto *srtp.Context, target *net.UDPAddr, packets, octets, timestamp *atomic.Uint32, track string) {
	if session.closed.Load() || packet == nil || conn == nil || crypto == nil || target == nil {
		return
	}

	clone := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Marker:         packet.Marker,
			PayloadType:    payloadType,
			SequenceNumber: packet.SequenceNumber,
			Timestamp:      packet.Timestamp,
			SSRC:           ssrc,
		},
		Payload: append([]byte(nil), packet.Payload...),
	}

	plain, err := clone.Marshal()
	if err != nil {
		m.logger.Debug("marshal homekit camera RTP", "entrypoint_id", m.entrypoint.ID, "track", track, "error", err)
		return
	}

	cipher, err := crypto.EncryptRTP(nil, plain, nil)
	if err != nil {
		m.logger.Debug("encrypt homekit camera SRTP", "entrypoint_id", m.entrypoint.ID, "track", track, "error", err)
		return
	}

	if _, err := conn.WriteToUDP(cipher, target); err != nil && !session.closed.Load() {
		m.logger.Debug("send homekit camera SRTP", "entrypoint_id", m.entrypoint.ID, "track", track, "error", err)
		return
	}

	payloadLength := len(packet.Payload)
	if uint64(payloadLength) > math.MaxUint32 {
		m.logger.Warn("HomeKit camera RTP payload exceeds counter range", "entrypoint_id", m.entrypoint.ID, "track", track, "bytes", payloadLength)
		return
	}

	packets.Add(1)
	octets.Add(uint32(payloadLength))
	timestamp.Store(packet.Timestamp)

	if packets.Load() == 1 {
		m.logger.Info("homekit camera first SRTP packet forwarded", "entrypoint_id", m.entrypoint.ID, "track", track)
	}
}

func (m *cameraSessionManager) sendRTCP(ctx context.Context, session *cameraSession) {
	ticker := time.NewTicker(cameraRTCPInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.sendRTCPSenderReport(session, session.videoConn, session.videoSRTP, session.videoTarget, session.videoSSRC, &session.videoPackets, &session.videoOctets, &session.videoTime)
			m.sendRTCPSenderReport(session, session.audioConn, session.audioSRTP, session.audioTarget, session.audioSSRC, &session.audioPackets, &session.audioOctets, &session.audioTime)
		}
	}
}

func (m *cameraSessionManager) sendRTCPSenderReport(session *cameraSession, conn *net.UDPConn, crypto *srtp.Context, target *net.UDPAddr, ssrc uint32, packets, octets, timestamp *atomic.Uint32) {
	if session.closed.Load() || conn == nil || crypto == nil || target == nil {
		return
	}

	report, err := (&rtcp.SenderReport{
		SSRC:        ssrc,
		NTPTime:     cameraNTPTime(time.Now()),
		RTPTime:     timestamp.Load(),
		PacketCount: packets.Load(),
		OctetCount:  octets.Load(),
	}).Marshal()
	if err != nil {
		return
	}

	cipher, err := crypto.EncryptRTCP(nil, report, nil)
	if err == nil {
		_, _ = conn.WriteToUDP(cipher, target)
	}
}

func (m *cameraSessionManager) setStreamingStatus(status byte) {
	m.service.StreamingStatus.SetValue(mustCameraTLV(cameraStreamingStatus{Status: status}))
}

func mustCameraTLV(value any) []byte {
	data, err := tlv8.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("encode HomeKit camera TLV: %v", err))
	}

	return data
}

func cameraCrypto() (cameraSRTPCryptoSuite, error) {
	key, err := cameraRandomBytes(cameraSRTPKeyLength)
	if err != nil {
		return cameraSRTPCryptoSuite{}, err
	}

	salt, err := cameraRandomBytes(cameraSRTPSaltLength)
	if err != nil {
		return cameraSRTPCryptoSuite{}, err
	}

	return cameraSRTPCryptoSuite{
		CryptoSuite: cameraCryptoAESCM128HMACSHA180,
		MasterKey:   string(key),
		MasterSalt:  string(salt),
	}, nil
}

// cameraSRTPContext encrypts the accessory's RTP with the key material that it
// returned in Setup Endpoints. The controller uses those response keys to
// decrypt the accessory-to-controller media direction.
func cameraSRTPContext(crypto cameraSRTPCryptoSuite) (*srtp.Context, error) {
	if len(crypto.MasterKey) != cameraSRTPKeyLength || len(crypto.MasterSalt) != cameraSRTPSaltLength {
		return nil, errors.New("invalid SRTP key material")
	}

	context, err := srtp.CreateContext([]byte(crypto.MasterKey), []byte(crypto.MasterSalt), srtp.ProtectionProfileAes128CmHmacSha1_80)
	if err != nil {
		return nil, fmt.Errorf("create SRTP context: %w", err)
	}

	return context, nil
}

func cameraRandomBytes(length int) ([]byte, error) {
	data := make([]byte, length)
	if _, err := rand.Read(data); err != nil {
		return nil, fmt.Errorf("generate random data: %w", err)
	}

	return data, nil
}

func cameraRandomUint32() (uint32, error) {
	data, err := cameraRandomBytes(4)
	if err != nil {
		return 0, err
	}

	return binary.BigEndian.Uint32(data), nil
}

func cameraUDPPort() (*net.UDPConn, uint16, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, 0, err
	}

	address, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		_ = conn.Close()
		return nil, 0, errors.New("unexpected local UDP address")
	}

	if address.Port < 0 || address.Port > math.MaxUint16 {
		_ = conn.Close()
		return nil, 0, errors.New("local UDP port is outside uint16 range")
	}

	return conn, uint16(address.Port), nil
}

func cameraNTPTime(now time.Time) uint64 {
	const ntpEpoch = 2208988800

	unixSeconds := now.Unix()
	unixSeconds = max(unixSeconds, 0)
	seconds := uint64(unixSeconds) + ntpEpoch
	nanoseconds := now.Nanosecond()
	nanoseconds = max(nanoseconds, 0)
	fraction := (uint64(nanoseconds) << 32) / uint64(time.Second)

	return (seconds << 32) | fraction
}

func sessionContext(session *cameraSession) context.Context {
	if session == nil || session.ctx == nil {
		return context.Background()
	}

	return session.ctx
}

func cameraLocalIP(request *http.Request, version byte) (net.IP, error) {
	if request == nil {
		return nil, errors.New("HomeKit request is unavailable")
	}

	if version != 0 {
		return nil, fmt.Errorf("unsupported HomeKit IP version %d", version)
	}

	address, ok := request.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || address == nil {
		return nil, errors.New("HomeKit request local address is unavailable")
	}

	host, _, err := net.SplitHostPort(address.String())
	if err != nil {
		return nil, fmt.Errorf("parse HomeKit local address: %w", err)
	}

	ip := net.ParseIP(host)
	if ip == nil || ip.To4() == nil {
		return nil, fmt.Errorf("HomeKit local address %q is not IPv4", host)
	}

	return ip, nil
}
