package systemcontrol

import (
	"context"
	"errors"
	"testing"

	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/system"
)

type managerStub struct {
	rebootCalls  int
	restartCalls []string
	statusCalls  []string
	statusByName map[string]system.ServiceStatus
	rebootErr    error
	restartErr   error
	statusErr    error
}

func (m *managerStub) RebootHost(context.Context) error {
	m.rebootCalls++
	return m.rebootErr
}

func (m *managerStub) Status(_ context.Context, serviceName string) (system.ServiceStatus, error) {
	m.statusCalls = append(m.statusCalls, serviceName)
	if m.statusErr != nil {
		return system.ServiceStatus{}, m.statusErr
	}
	if m.statusByName != nil {
		if status, ok := m.statusByName[serviceName]; ok {
			return status, nil
		}
	}
	return system.ServiceStatus{Name: serviceName, Running: true}, nil
}

func (m *managerStub) Restart(_ context.Context, serviceName string) error {
	m.restartCalls = append(m.restartCalls, serviceName)
	return m.restartErr
}

func TestServicePolicyGatesReboot(t *testing.T) {
	stub := &managerStub{}
	svc := New(stub, false, map[string]config.SystemServiceConfig{"dropbear": {Enabled: true, Exposed: true}})

	if err := svc.Reboot(context.Background()); !errors.Is(err, ErrRebootDisabled) {
		t.Fatalf("expected reboot disabled error, got %v", err)
	}
}

func TestServiceAllowsConfiguredService(t *testing.T) {
	stub := &managerStub{}
	svc := New(stub, true, map[string]config.SystemServiceConfig{"dropbear": {Enabled: true, Exposed: true}})

	if err := svc.RestartService(context.Background(), "dbus"); !errors.Is(err, ErrServiceNotExposed) {
		t.Fatalf("expected service not exposed, got %v", err)
	}
	if err := svc.RestartService(context.Background(), "dropbear"); err != nil {
		t.Fatalf("restart configured service failed: %v", err)
	}
	if len(stub.restartCalls) != 1 || stub.restartCalls[0] != "dropbear" {
		t.Fatalf("unexpected restart calls: %+v", stub.restartCalls)
	}
}

func TestServiceStatusDisabledService(t *testing.T) {
	stub := &managerStub{}
	svc := New(stub, true, map[string]config.SystemServiceConfig{"dropbear": {Enabled: false, Exposed: true}})

	_, err := svc.ServiceStatus(context.Background(), "dropbear")
	if !errors.Is(err, ErrServiceDisabled) {
		t.Fatalf("expected service disabled, got %v", err)
	}
}

func TestServiceStatusPassThrough(t *testing.T) {
	stub := &managerStub{
		statusByName: map[string]system.ServiceStatus{
			"dropbear": {Name: "dropbear", Running: true},
		},
	}
	svc := New(stub, true, map[string]config.SystemServiceConfig{"dropbear": {Enabled: true, Exposed: true}})

	status, err := svc.ServiceStatus(context.Background(), "dropbear")
	if err != nil {
		t.Fatalf("status failed: %v", err)
	}
	if !status.Running || status.Name != "dropbear" {
		t.Fatalf("unexpected status: %+v", status)
	}
}
