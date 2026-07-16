package system

import (
	"context"
	"fmt"
	"os/exec"
)

const shutdownPath = "/sbin/shutdown"

// CommandRunner isolates process execution so rebooting can be tested safely.
type CommandRunner interface {
	Run(context.Context, string, ...string) error
}

type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, name string, args ...string) error {
	return exec.CommandContext(ctx, name, args...).Run()
}

type RebootAdapter struct {
	runner CommandRunner
}

func NewRebootAdapter(runner CommandRunner) *RebootAdapter {
	if runner == nil {
		runner = execCommandRunner{}
	}
	return &RebootAdapter{runner: runner}
}

func (r *RebootAdapter) Reboot(ctx context.Context) error {
	if r == nil || r.runner == nil {
		return ErrRuntimeUnavailable
	}
	if err := r.runner.Run(ctx, shutdownPath, "-r", "now"); err != nil {
		return fmt.Errorf("reboot system: %w", err)
	}
	return nil
}
