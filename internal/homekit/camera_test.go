package homekit

import (
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"bticino-go-companion/internal/media"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/brutella/hap/accessory"
	"github.com/brutella/hap/characteristic"
	"github.com/brutella/hap/service"
	"github.com/brutella/hap/tlv8"
	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
)

func TestManager_BuildsVideoDoorbellForStreamCapableEntrypoint(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(testConfigStore(t))
	if err != nil {
		t.Fatal(err)
	}
	manager.SetStreamCoordinator(testCameraCoordinator())
	cfg := testConfigStore(t).Snapshot()
	cfg.Companion.Entrypoints = []config.Entrypoint{
		{ID: "gate", Label: "Gate", Capabilities: config.Capabilities{Stream: true}},
		{ID: "side", Label: "Side", Capabilities: config.Capabilities{Stream: false}},
	}

	manager.mu.Lock()
	_, accessories := manager.buildAccessoriesLocked(cfg, newRuntimeConfig(cfg))
	manager.mu.Unlock()

	if len(accessories) != 1 {
		t.Fatalf("doorbell accessory count = %d, want 1", len(accessories))
	}
	doorbell := manager.doorbells["gate"]
	if doorbell == nil {
		t.Fatal("stream-capable entrypoint did not create a HomeKit video doorbell")
	}
	if doorbell.aid == 0 {
		t.Fatal("doorbell accessory id was not assigned")
	}
	if accessories[0].Type != accessory.TypeVideoDoorbell {
		t.Fatalf("accessory type = %d, want video doorbell", accessories[0].Type)
	}
	if len(accessories[0].Ss) != 4 {
		t.Fatalf("doorbell service count = %d, want accessory info, doorbell, camera control, and stream management", len(accessories[0].Ss))
	}
	if !accessories[0].Ss[1].Primary {
		t.Fatal("doorbell service is not primary")
	}
	if got := accessories[0].Ss[2].C(characteristic.TypeOn).Value(); got != true {
		t.Fatalf("camera control is enabled = %v, want true", got)
	}
	if got := accessories[0].Ss[3].C(characteristic.TypeActive).Value(); got != characteristic.ActiveActive {
		t.Fatalf("camera stream management active = %v, want %d", got, characteristic.ActiveActive)
	}

	var status cameraStreamingStatus
	if err := tlv8.Unmarshal(doorbell.stream.service.StreamingStatus.Value(), &status); err != nil {
		t.Fatalf("decode camera streaming status: %v", err)
	}
	if status.Status != cameraStreamingAvailable {
		t.Fatalf("camera streaming status = %d, want available", status.Status)
	}
}

func TestCameraSession_SetupEndpointsUpdatesCharacteristicResponse(t *testing.T) {
	t.Parallel()

	cameraService := service.NewCameraRTPStreamManagement()
	manager := newCameraSessionManager(testCameraCoordinator(), config.Entrypoint{ID: "gate"}, cameraService, nil)
	setup := mustCameraTLV(cameraSetupEndpointsRequest{
		SessionID: "session",
		Address:   cameraAddress{IPVersion: 0, IPAddr: "10.0.0.113", VideoRTPPort: 5000, AudioRTPPort: 5001},
		VideoCrypto: cameraSRTPCryptoSuite{
			CryptoSuite: cameraCryptoAESCM128HMACSHA180,
			MasterKey:   string(make([]byte, cameraSRTPKeyLength)),
			MasterSalt:  string(make([]byte, cameraSRTPSaltLength)),
		},
		AudioCrypto: cameraSRTPCryptoSuite{
			CryptoSuite: cameraCryptoAESCM128HMACSHA180,
			MasterKey:   string(make([]byte, cameraSRTPKeyLength)),
			MasterSalt:  string(make([]byte, cameraSRTPSaltLength)),
		},
	})
	req := (&http.Request{}).WithContext(context.WithValue(context.Background(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("10.0.0.143"), Port: 51826}))
	if _, status := cameraService.SetupEndpoints.C.SetValueRequest(base64.StdEncoding.EncodeToString(setup), req); status != 0 {
		t.Fatalf("setup endpoints status = %d, want success", status)
	}

	var response cameraSetupEndpointsResponse
	if err := tlv8.Unmarshal(cameraService.SetupEndpoints.Value(), &response); err != nil {
		t.Fatalf("decode setup endpoints response: %v", err)
	}
	if response.SessionID != "session" || response.Address.IPAddr != "10.0.0.143" || response.Address.VideoRTPPort == 0 || response.Address.AudioRTPPort == 0 {
		t.Fatalf("setup endpoints response = %#v", response)
	}
	plain, err := (&rtp.Packet{Header: rtp.Header{Version: 2, SSRC: 42}, Payload: []byte("video")}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := manager.active.videoSRTP.EncryptRTP(nil, plain, nil)
	if err != nil {
		t.Fatal(err)
	}
	controllerContext, err := cameraSRTPContext(response.VideoCrypto)
	if err != nil {
		t.Fatal(err)
	}
	decrypted, err := controllerContext.DecryptRTP(nil, cipher, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decrypted, plain) {
		t.Fatalf("decrypted RTP = %x, want %x", decrypted, plain)
	}
	manager.clearSession(manager.active)
}

func TestCameraSession_StartAndReleaseCoordinatorLease(t *testing.T) {
	t.Parallel()

	coordinator := testCameraCoordinator()
	cameraService := service.NewCameraRTPStreamManagement()
	manager := newCameraSessionManager(coordinator, config.Entrypoint{ID: "gate", DevAddr: "20", Capabilities: config.Capabilities{Stream: true}}, cameraService, nil)
	ctx, cancel := context.WithCancel(context.Background())
	session := &cameraSession{id: "session", ctx: ctx, cancel: cancel}
	manager.active = session

	if err := manager.start(session, cameraSelectedStreamConfiguration{}); err != nil {
		t.Fatalf("start camera session: %v", err)
	}
	if got := coordinator.Snapshot().Owner; got != media.StreamOwnerCompanion {
		t.Fatalf("coordinator owner = %q, want companion", got)
	}

	manager.clearSession(session)
	if got := coordinator.Snapshot().Owner; got != media.StreamOwnerIdle {
		t.Fatalf("coordinator owner after close = %q, want idle", got)
	}
}

func TestCameraSession_RejectsInvalidEndpointRequest(t *testing.T) {
	t.Parallel()

	manager := newCameraSessionManager(testCameraCoordinator(), config.Entrypoint{ID: "gate"}, service.NewCameraRTPStreamManagement(), nil)
	request, err := tlv8.Marshal(cameraSetupEndpointsRequest{SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.setupEndpoints(request, nil); err == nil {
		t.Fatal("setupEndpoints accepted a request without controller endpoints")
	}
}

func TestCameraCapabilitiesUseSingleByteProfileLevelAndSampleRate(t *testing.T) {
	t.Parallel()

	manager := newCameraSessionManager(testCameraCoordinator(), config.Entrypoint{ID: "gate"}, service.NewCameraRTPStreamManagement(), nil)
	video := manager.service.SupportedVideoStreamConfiguration.Value()
	if !bytes.Contains(video, []byte{1, 1, 1, 2, 1, 0}) {
		t.Fatalf("video capability does not contain one-byte profile and level: %x", video)
	}
	audio := manager.service.SupportedAudioStreamConfiguration.Value()
	if !bytes.Contains(audio, []byte{3, 1, 1}) {
		t.Fatalf("audio capability does not contain a one-byte sample rate: %x", audio)
	}
}

func testCameraCoordinator() *media.StreamCoordinator {
	return media.NewStreamCoordinator(nil, func(_ config.Entrypoint, _ media.SourceEvents) (media.ManagedSource, func(), error) {
		return cameraManagedSource{}, nil, nil
	})
}

type cameraManagedSource struct{}

func (cameraManagedSource) Start(context.Context) error { return nil }
func (cameraManagedSource) Close(context.Context) error { return nil }

var _ media.ManagedSource = cameraManagedSource{}

func TestCameraSession_RejectsUnknownStreamCommand(t *testing.T) {
	t.Parallel()

	manager := newCameraSessionManager(testCameraCoordinator(), config.Entrypoint{ID: "gate"}, service.NewCameraRTPStreamManagement(), nil)
	manager.active = &cameraSession{id: "session", ctx: context.Background()}
	err := manager.handleSelectedStreamConfiguration(mustCameraTLV(cameraSelectedStreamConfiguration{
		Control: cameraSessionControl{SessionID: "session", Command: 99},
	}))
	if err == nil {
		t.Fatalf("unknown stream command error = %v, want error", err)
	}
}

func TestCameraSRTPContextUsesProvidedKeyMaterial(t *testing.T) {
	t.Parallel()

	crypto := cameraSRTPCryptoSuite{
		CryptoSuite: cameraCryptoAESCM128HMACSHA180,
		MasterKey:   string([]byte("0123456789abcdef")),
		MasterSalt:  string([]byte("0123456789abcd")),
	}
	accessoryContext, err := cameraSRTPContext(crypto)
	if err != nil {
		t.Fatal(err)
	}
	controllerContext, err := srtp.CreateContext([]byte(crypto.MasterKey), []byte(crypto.MasterSalt), srtp.ProtectionProfileAes128CmHmacSha1_80)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := (&rtp.Packet{Header: rtp.Header{Version: 2, SSRC: 42}, Payload: []byte("video")}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := accessoryContext.EncryptRTP(nil, plain, nil)
	if err != nil {
		t.Fatal(err)
	}
	decrypted, err := controllerContext.DecryptRTP(nil, cipher, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decrypted, plain) {
		t.Fatalf("decrypted RTP = %x, want %x", decrypted, plain)
	}
}

func TestManager_ResourceHandlerServesDoorbellSnapshot(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(testConfigStore(t))
	if err != nil {
		t.Fatal(err)
	}
	manager.SetStreamCoordinator(testCameraCoordinator())
	cfg := testConfigStore(t).Snapshot()
	cfg.Companion.Entrypoints = []config.Entrypoint{{ID: "gate", Label: "Gate", Capabilities: config.Capabilities{Stream: true}}}
	manager.mu.Lock()
	_, _ = manager.buildAccessoriesLocked(cfg, newRuntimeConfig(cfg))
	aid := manager.doorbells["gate"].aid
	manager.mu.Unlock()
	provider := &cameraSnapshotProvider{image: []byte{0xff, 0xd8, 0xff, 0xd9}}
	manager.SetSnapshotProvider(provider)

	req := httptest.NewRequest(http.MethodPost, "/resource", bytes.NewBufferString(`{"aid":`+fmt.Sprint(aid)+`,"resource-type":"image"}`))
	response := httptest.NewRecorder()
	manager.resourceHandler(func(*http.Request) bool { return true })(response, req)

	if response.Code != http.StatusOK {
		t.Fatalf("resource status = %d, want %d", response.Code, http.StatusOK)
	}
	if got := response.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Fatalf("content type = %q, want image/jpeg", got)
	}
	if !bytes.Equal(response.Body.Bytes(), provider.image) || provider.entrypointID != "gate" {
		t.Fatalf("resource response = %x, entrypoint = %q", response.Body.Bytes(), provider.entrypointID)
	}
}

func TestManager_ResourceHandlerRejectsUnauthorizedAndUnavailableRequests(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(testConfigStore(t))
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name       string
		authorized bool
		body       string
		wantStatus int
	}{
		{name: "unauthorized", authorized: false, body: `{"aid":1,"resource-type":"image"}`, wantStatus: http.StatusUnauthorized},
		{name: "unknown accessory", authorized: true, body: `{"aid":1,"resource-type":"image"}`, wantStatus: http.StatusNotFound},
		{name: "invalid resource type", authorized: true, body: `{"aid":1,"resource-type":"video"}`, wantStatus: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/resource", bytes.NewBufferString(test.body))
			response := httptest.NewRecorder()
			manager.resourceHandler(func(*http.Request) bool { return test.authorized })(response, req)
			if response.Code != test.wantStatus {
				t.Fatalf("resource status = %d, want %d", response.Code, test.wantStatus)
			}
		})
	}
}

func TestManager_SyncPublishesOneDoorbellPressPerPhysicalRing(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(testConfigStore(t))
	if err != nil {
		t.Fatal(err)
	}
	manager.SetStreamCoordinator(testCameraCoordinator())
	cfg := testConfigStore(t).Snapshot()
	cfg.Companion.Entrypoints = []config.Entrypoint{{ID: "gate", Label: "Gate", Capabilities: config.Capabilities{Stream: true, Ring: true}}}
	manager.mu.Lock()
	_, _ = manager.buildAccessoriesLocked(cfg, newRuntimeConfig(cfg))
	doorbell := manager.doorbells["gate"].doorbell
	manager.mu.Unlock()

	presses := 0
	doorbell.ProgrammableSwitchEvent.OnCValueUpdate(func(_ *characteristic.C, _, _ interface{}, _ *http.Request) {
		presses++
	})
	manager.Sync(core.State{PhysicalRing: &core.PhysicalRing{EntrypointID: "gate"}})
	manager.Sync(core.State{PhysicalRing: &core.PhysicalRing{EntrypointID: "gate"}})
	manager.Sync(core.State{})
	manager.Sync(core.State{PhysicalRing: &core.PhysicalRing{EntrypointID: "gate"}})

	if presses != 2 {
		t.Fatalf("doorbell presses = %d, want 2", presses)
	}
}

type cameraSnapshotProvider struct {
	image        []byte
	err          error
	entrypointID string
}

func (p *cameraSnapshotProvider) Latest(entrypointID string) ([]byte, error) {
	p.entrypointID = entrypointID
	return p.image, p.err
}
