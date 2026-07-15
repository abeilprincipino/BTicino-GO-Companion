package discovery

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/grandcat/zeroconf"
)

const (
	ServiceType = "_bticomp._tcp"
	Domain      = "local."
)

var ErrInvalidDeviceID = errors.New("invalid mDNS device ID")

type Advertisement struct {
	DeviceID   string
	Name       string
	Model      string
	NeedsClaim bool
	Port       int
	Interfaces []net.Interface
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
}

func NewService(registrar Registrar) *Service {
	if registrar == nil {
		registrar = zeroconfRegistrar{}
	}

	return &Service{registrar: registrar}
}

func (s *Service) Advertise(advertisement Advertisement) (Registration, error) {
	request, err := registrationRequest(advertisement)
	if err != nil {
		return nil, err
	}

	return s.registrar.Register(request)
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
		"name=" + strings.TrimSpace(advertisement.Name),
		"model=" + strings.TrimSpace(advertisement.Model),
		"needs_claim=" + strconv.FormatBool(advertisement.NeedsClaim),
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
