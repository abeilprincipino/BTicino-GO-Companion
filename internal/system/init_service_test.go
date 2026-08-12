package system

import (
	"context"
	"testing"
)

func TestInitScriptCommandRunnerRejectsNonInitPath(t *testing.T) {
	t.Parallel()

	err := (initScriptCommandRunner{}).Run(context.Background(), shutdownPath, "-r", "now")
	if err == nil {
		t.Fatal("Run() error = nil")
	}
}

func TestInitScriptAdapterUsesProvidedRunner(t *testing.T) {
	t.Parallel()

	runner := &recordingCommandRunner{}
	adapter := NewInitScriptAdapter(runner)

	if err := adapter.Restart(context.Background(), testServiceName); err != nil {
		t.Fatal(err)
	}

	if runner.name != initDirectory+testServiceName || len(runner.args) != 1 || runner.args[0] != "restart" {
		t.Fatalf("command = %q %#v", runner.name, runner.args)
	}
}

type recordingCommandRunner struct {
	name string
	args []string
}

func (r *recordingCommandRunner) Run(_ context.Context, name string, args ...string) error {
	r.name = name
	r.args = args
	return nil
}
