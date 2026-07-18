package discovery

import (
	"bticino-go-companion/internal/config"
	"context"
	"errors"
	"net"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

type recordingRegistrar struct {
	request RegistrationRequest
	err     error
	called  bool
}

func (r *recordingRegistrar) Register(request RegistrationRequest) (Registration, error) {
	r.called = true

	r.request = request
	if r.err != nil {
		return nil, r.err
	}

	return registrationStub{}, nil
}

type registrationStub struct{}

func (registrationStub) Shutdown() {}

type notifyingRegistrar struct{ registrations chan RegistrationRequest }

func (r notifyingRegistrar) Register(request RegistrationRequest) (Registration, error) {
	r.registrations <- request
	return registrationStub{}, nil
}

func TestServiceAdvertiseUsesDeviceIDAsMDNSHost(t *testing.T) {
	t.Parallel()

	registrar := &recordingRegistrar{}
	service := NewService(registrar)
	interfaces := []net.Interface{{Name: "eth0"}}

	registration, err := service.Advertise(Advertisement{
		DeviceID:     "c300x-aabbccddeeff",
		Model:        "C300X",
		PairingState: config.PairingStateClaimable,
		InstanceID:   "00112233445566778899aabbccddeeff",
		Port:         8080,
		Interfaces:   interfaces,
	})
	if err != nil {
		t.Fatalf("advertise: %v", err)
	}

	if registration == nil {
		t.Fatal("expected registration")
	}

	request := registrar.request
	if request.Instance != "c300x-aabbccddeeff" {
		t.Fatalf("instance = %q", request.Instance)
	}

	if request.Hostname != "c300x-aabbccddeeff" || request.Domain != "local." {
		t.Fatalf("SRV target inputs = host %q domain %q; want c300x-aabbccddeeff.local.", request.Hostname, request.Domain)
	}

	if request.Service != ServiceType || request.Port != 8080 {
		t.Fatalf("unexpected service request: %+v", request)
	}

	if len(request.Interfaces) != 1 || request.Interfaces[0].Name != interfaces[0].Name {
		t.Fatalf("interfaces = %+v, want %+v", request.Interfaces, interfaces)
	}

	wantTXT := []string{
		"device_id=c300x-aabbccddeeff",
		"api=v3",
		"scheme=http",
		"model=C300X",
		"pairing_state=claimable",
		"instance_id=00112233445566778899aabbccddeeff",
	}
	if !slices.Equal(request.TXT, wantTXT) {
		t.Fatalf("TXT = %#v, want %#v", request.TXT, wantTXT)
	}
}

func TestServiceAdvertiseRejectsInvalidDeviceIDWithoutRegistration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		deviceID string
	}{
		{name: "empty", deviceID: ""},
		{name: "leading hyphen", deviceID: "-c300x"},
		{name: "trailing hyphen", deviceID: "c300x-"},
		{name: "contains dot", deviceID: "c300x.local"},
		{name: "contains space", deviceID: "c300x aabb"},
		{name: "too long", deviceID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			registrar := &recordingRegistrar{}

			_, err := NewService(registrar).Advertise(Advertisement{DeviceID: test.deviceID, Port: 8080})
			if !errors.Is(err, ErrInvalidDeviceID) {
				t.Fatalf("got error %v, want ErrInvalidDeviceID", err)
			}

			if registrar.called {
				t.Fatalf("registrar called with %+v", registrar.request)
			}
		})
	}
}

func TestServiceAdvertiseRejectsInvalidPort(t *testing.T) {
	t.Parallel()

	registrar := &recordingRegistrar{}

	_, err := NewService(registrar).Advertise(Advertisement{DeviceID: "c300x-aabb", Port: 0})
	if err == nil {
		t.Fatal("expected invalid port error")
	}

	if registrar.called {
		t.Fatalf("registrar called with %+v", registrar.request)
	}
}

func TestServiceRunReadvertisesChangedPairingState(t *testing.T) {
	registrar := notifyingRegistrar{registrations: make(chan RegistrationRequest, 2)}
	service := NewService(registrar)
	service.refresh = time.Millisecond
	var claimed atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = service.Run(ctx, func() (Advertisement, error) {
			state := config.PairingStateClaimable
			if claimed.Load() {
				state = config.PairingStateClaimed
			}
			return Advertisement{DeviceID: "c300x-aabb", PairingState: state, InstanceID: "00112233445566778899aabbccddeeff", Port: 8080}, nil
		})
	}()

	first := <-registrar.registrations
	claimed.Store(true)
	second := <-registrar.registrations
	if !slices.Contains(first.TXT, "pairing_state=claimable") || !slices.Contains(second.TXT, "pairing_state=claimed") {
		t.Fatalf("pairing TXT transition = %#v -> %#v", first.TXT, second.TXT)
	}
}
