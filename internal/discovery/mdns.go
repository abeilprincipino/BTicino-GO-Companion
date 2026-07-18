package discovery

import (
	"bticino-go-companion/internal/config"
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/grandcat/zeroconf"
)

const (
	ServiceType  = "_bticomp._tcp"
	Domain       = "local."
	refreshEvery = 15 * time.Second
	retryInitial = time.Second
	retryMaximum = 30 * time.Second
)

var ErrInvalidDeviceID = errors.New("invalid mDNS device ID")

type Advertisement struct {
	DeviceID     string
	Model        string
	PairingState config.PairingState
	InstanceID   string
	Port         int
	Interfaces   []net.Interface
}

type Registration interface {
	Shutdown()
}

type Registrar interface {
	Register(RegistrationRequest) (Registration, error)
}

type RegistrationRequest struct {
	Instance   string
	Service    string
	Domain     string
	Hostname   string
	Port       int
	TXT        []string
	Interfaces []net.Interface
}

type Service struct {
	registrar Registrar
	refresh   time.Duration
	retryMax  time.Duration
}

func NewService(registrar Registrar) *Service {
	if registrar == nil {
		registrar = zeroconfRegistrar{}
	}

	return &Service{registrar: registrar, refresh: refreshEvery, retryMax: retryMaximum}
}

func (s *Service) Advertise(advertisement Advertisement) (Registration, error) {
	request, err := registrationRequest(advertisement)
	if err != nil {
		return nil, err
	}

	return s.registrar.Register(request)
}

func (s *Service) Run(ctx context.Context, snapshot func() (Advertisement, error)) error {
	if snapshot == nil {
		return errors.New("mDNS snapshot is unavailable")
	}

	ticker := time.NewTicker(s.refresh)
	defer ticker.Stop()

	backoff := retryInitial
	var current Advertisement
	var registration Registration
	for {
		if registration == nil {
			next, err := snapshot()
			if err == nil {
				registration, err = s.Advertise(next)
			}
			if err == nil {
				current = next
				backoff = retryInitial
			} else {
				if err := wait(ctx, backoff); err != nil {
					return err
				}
				backoff = min(backoff*2, s.retryMax)
				continue
			}
		}

		select {
		case <-ctx.Done():
			registration.Shutdown()
			return nil
		case <-ticker.C:
			next, err := snapshot()
			if err != nil || advertisementsEqual(current, next) {
				continue
			}
			registration.Shutdown()
			registration = nil
		}
	}
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func advertisementsEqual(left, right Advertisement) bool {
	return left.DeviceID == right.DeviceID && left.Model == right.Model && left.PairingState == right.PairingState && left.InstanceID == right.InstanceID && left.Port == right.Port && slices.EqualFunc(left.Interfaces, right.Interfaces, func(a, b net.Interface) bool { return a.Index == b.Index && a.Name == b.Name })
}

func registrationRequest(advertisement Advertisement) (RegistrationRequest, error) {
	if err := validateHostname(advertisement.DeviceID); err != nil {
		return RegistrationRequest{}, err
	}

	if advertisement.Port < 1 || advertisement.Port > 65535 {
		return RegistrationRequest{}, fmt.Errorf("invalid mDNS port %d", advertisement.Port)
	}

	return RegistrationRequest{
		Instance:   advertisement.DeviceID,
		Service:    ServiceType,
		Domain:     Domain,
		Hostname:   advertisement.DeviceID,
		Port:       advertisement.Port,
		TXT:        txtRecords(advertisement),
		Interfaces: append([]net.Interface{}, advertisement.Interfaces...),
	}, nil
}

func txtRecords(advertisement Advertisement) []string {
	return []string{
		"device_id=" + advertisement.DeviceID,
		"api=v3",
		"scheme=http",
		"model=" + strings.TrimSpace(advertisement.Model),
		"pairing_state=" + string(advertisement.PairingState),
		"instance_id=" + strings.TrimSpace(advertisement.InstanceID),
	}
}

func validateHostname(hostname string) error {
	if len(hostname) == 0 || len(hostname) > 63 {
		return fmt.Errorf("%w: %q", ErrInvalidDeviceID, hostname)
	}

	for index, character := range hostname {
		isLetter := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z'

		isDigit := character >= '0' && character <= '9'
		if !isLetter && !isDigit && character != '-' {
			return fmt.Errorf("%w: %q", ErrInvalidDeviceID, hostname)
		}

		if character == '-' && (index == 0 || index == len(hostname)-1) {
			return fmt.Errorf("%w: %q", ErrInvalidDeviceID, hostname)
		}
	}

	return nil
}

type zeroconfRegistrar struct{}

func (zeroconfRegistrar) Register(request RegistrationRequest) (Registration, error) {
	server, err := zeroconf.RegisterProxy(
		request.Instance,
		request.Service,
		request.Domain,
		request.Port,
		request.Hostname,
		nil,
		request.TXT,
		request.Interfaces,
	)
	if err != nil {
		return nil, err
	}

	return server, nil
}
