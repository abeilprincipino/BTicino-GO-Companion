package system

import (
	"context"
	"errors"
	"testing"
)

func TestRebootAdapter_UsesIntercomShutdownCommand(t *testing.T) {
	t.Parallel()

	runner := &rebootRunner{}
	if err := NewRebootAdapter(runner).Reboot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.name != shutdownPath || len(runner.args) != 2 || runner.args[0] != "-r" || runner.args[1] != "now" {
		t.Fatalf("command = %q %#v", runner.name, runner.args)
	}
}

func TestRebootAdapter_WrapsRunnerError(t *testing.T) {
	t.Parallel()

	errRunner := &rebootRunner{err: errors.New("failed")}
	if err := NewRebootAdapter(errRunner).Reboot(context.Background()); !errors.Is(err, errRunner.err) {
		t.Fatalf("Reboot() error = %v", err)
	}
}

type rebootRunner struct {
	name string
	args []string
	err  error
}

func (r *rebootRunner) Run(_ context.Context, name string, args ...string) error {
	r.name = name
	r.args = args
	return r.err
}
