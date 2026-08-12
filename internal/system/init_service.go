package system

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

const initDirectory = "/etc/init.d/"

// InitScriptAdapter controls allowlisted SysV init scripts through RuntimeControl.
type InitScriptAdapter struct {
	runner CommandRunner
}

func NewInitScriptAdapter(runner CommandRunner) *InitScriptAdapter {
	if runner == nil {
		runner = initScriptCommandRunner{}
	}

	return &InitScriptAdapter{runner: runner}
}

type initScriptCommandRunner struct{}

func (initScriptCommandRunner) Run(ctx context.Context, name string, args ...string) error {
	if !strings.HasPrefix(name, initDirectory) || strings.TrimPrefix(name, initDirectory) == "" || strings.Contains(strings.TrimPrefix(name, initDirectory), "/") {
		return fmt.Errorf("unsupported init command %q", name)
	}

	return exec.CommandContext(ctx, name, args...).Run() // #nosec G204 -- service names are allowlisted by RuntimeControl and the executable is constrained to initDirectory.
}

func (s *InitScriptAdapter) Status(ctx context.Context, service string) (ServiceStatus, error) {
	err := s.runner.Run(ctx, initDirectory+service, "status")
	return ServiceStatus{Name: service, Running: err == nil}, err
}

func (s *InitScriptAdapter) Restart(ctx context.Context, service string) error {
	return s.runner.Run(ctx, initDirectory+service, "restart")
}
