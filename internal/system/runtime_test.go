package system

import (
	"context"
	"errors"
	"testing"
)

const testServiceName = "companion"

func TestRuntimeControl_ServiceAllowlist(t *testing.T) {
	t.Parallel()

	operator := &testServiceOperator{status: ServiceStatus{Name: testServiceName, Running: true}}
	control := NewRuntimeControl(operator, &testRebooter{}, []string{testServiceName})

	status, err := control.Status(context.Background(), testServiceName)
	if err != nil {
		t.Fatal(err)
	}

	if status != operator.status || operator.statusService != testServiceName {
		t.Fatalf("status = %#v, service = %q", status, operator.statusService)
	}

	if err := control.Restart(context.Background(), testServiceName); err != nil {
		t.Fatal(err)
	}

	if operator.restartService != testServiceName {
		t.Fatalf("restart service = %q", operator.restartService)
	}

	if _, err := control.Status(context.Background(), "dropbear"); !errors.Is(err, ErrServiceNotAllowed) {
		t.Fatalf("Status() error = %v", err)
	}

	if err := control.Restart(context.Background(), "dropbear"); !errors.Is(err, ErrServiceNotAllowed) {
		t.Fatalf("Restart() error = %v", err)
	}
}

func TestRuntimeControl_RebootIsInjected(t *testing.T) {
	t.Parallel()

	rebooter := &testRebooter{}

	control := NewRuntimeControl(nil, rebooter, nil)
	if err := control.Reboot(context.Background()); err != nil {
		t.Fatal(err)
	}

	if !rebooter.called {
		t.Fatal("rebooter was not called")
	}
	if !control.RebootAvailable() {
		t.Fatal("RebootAvailable() = false")
	}
}

func TestRuntimeControl_UnavailableDependencies(t *testing.T) {
	t.Parallel()

	control := NewRuntimeControl(nil, nil, []string{testServiceName})
	if _, err := control.Status(context.Background(), testServiceName); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("Status() error = %v", err)
	}

	if err := control.Restart(context.Background(), testServiceName); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("Restart() error = %v", err)
	}

	if err := control.Reboot(context.Background()); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("Reboot() error = %v", err)
	}
	if control.RebootAvailable() {
		t.Fatal("RebootAvailable() = true")
	}
}

type testServiceOperator struct {
	status         ServiceStatus
	statusService  string
	restartService string
}

func (o *testServiceOperator) Status(_ context.Context, service string) (ServiceStatus, error) {
	o.statusService = service
	return o.status, nil
}

func (o *testServiceOperator) Restart(_ context.Context, service string) error {
	o.restartService = service
	return nil
}

type testRebooter struct {
	called bool
}

func (r *testRebooter) Reboot(context.Context) error {
	r.called = true
	return nil
}
