package system

import "context"

const initDirectory = "/etc/init.d/"

// InitServiceAdapter controls allowlisted init scripts through RuntimeControl.
type InitServiceAdapter struct {
	runner CommandRunner
}

func NewInitServiceAdapter(runner CommandRunner) *InitServiceAdapter {
	if runner == nil {
		runner = execCommandRunner{}
	}
	return &InitServiceAdapter{runner: runner}
}

func (s *InitServiceAdapter) Status(ctx context.Context, service string) (ServiceStatus, error) {
	err := s.runner.Run(ctx, initDirectory+service, "status")
	return ServiceStatus{Name: service, Running: err == nil}, err
}

func (s *InitServiceAdapter) Restart(ctx context.Context, service string) error {
	return s.runner.Run(ctx, initDirectory+service, "restart")
}
