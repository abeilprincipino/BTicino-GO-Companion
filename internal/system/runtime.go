package system

import (
	"context"
	"errors"
)

var (
	ErrServiceNotAllowed  = errors.New("system: service is not allowed")
	ErrRuntimeUnavailable = errors.New("system: runtime control is unavailable")
)

type ServiceStatus struct {
	Name    string
	Running bool
}

type ServiceOperator interface {
	Status(context.Context, string) (ServiceStatus, error)
	Restart(context.Context, string) error
}

type Rebooter interface {
	Reboot(context.Context) error
}

type RuntimeControl struct {
	services ServiceOperator
	rebooter Rebooter
	allowed  map[string]struct{}
}

func NewRuntimeControl(services ServiceOperator, rebooter Rebooter, allowedServices []string) *RuntimeControl {
	allowed := make(map[string]struct{}, len(allowedServices))
	for _, service := range allowedServices {
		allowed[service] = struct{}{}
	}

	return &RuntimeControl{services: services, rebooter: rebooter, allowed: allowed}
}

func (r *RuntimeControl) Status(ctx context.Context, service string) (ServiceStatus, error) {
	if !r.isAllowed(service) {
		return ServiceStatus{}, ErrServiceNotAllowed
	}

	if r.services == nil {
		return ServiceStatus{}, ErrRuntimeUnavailable
	}

	return r.services.Status(ctx, service)
}

func (r *RuntimeControl) Restart(ctx context.Context, service string) error {
	if !r.isAllowed(service) {
		return ErrServiceNotAllowed
	}

	if r.services == nil {
		return ErrRuntimeUnavailable
	}

	return r.services.Restart(ctx, service)
}

func (r *RuntimeControl) Reboot(ctx context.Context) error {
	if r.rebooter == nil {
		return ErrRuntimeUnavailable
	}

	return r.rebooter.Reboot(ctx)
}

func (r *RuntimeControl) RebootAvailable() bool {
	return r != nil && r.rebooter != nil
}

func (r *RuntimeControl) isAllowed(service string) bool {
	_, ok := r.allowed[service]
	return ok
}
