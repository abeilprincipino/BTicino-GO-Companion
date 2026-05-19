package system

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizeServiceName(t *testing.T) {
	name, err := normalizeServiceName(" DropBear ")
	if err != nil {
		t.Fatalf("normalizeServiceName returned error: %v", err)
	}
	if name != "dropbear" {
		t.Fatalf("expected dropbear, got %q", name)
	}

	if _, err := normalizeServiceName(""); err == nil {
		t.Fatal("expected error for empty service name")
	}
	if _, err := normalizeServiceName("bad/name"); err == nil {
		t.Fatal("expected error for invalid service name")
	}
}

func TestInitScriptPath(t *testing.T) {
	got := initScriptPath("dropbear")
	want := "/etc/init.d/dropbear"
	if got != want {
		t.Fatalf("initScriptPath mismatch: got %q want %q", got, want)
	}
}

func TestRunExecCommand(t *testing.T) {
	out, err := runExecCommand(context.Background(), "sh", "-c", "printf test-output")
	if err != nil {
		t.Fatalf("runExecCommand returned error: %v", err)
	}
	if out != "test-output" {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestReadPID(t *testing.T) {
	base := t.TempDir()
	okPath := filepath.Join(base, "ok.pid")
	if err := os.WriteFile(okPath, []byte("123\n"), 0o644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}
	pid, err := readPID(okPath)
	if err != nil {
		t.Fatalf("readPID returned error: %v", err)
	}
	if pid != 123 {
		t.Fatalf("expected pid 123, got %d", pid)
	}

	for name, value := range map[string]string{
		"empty":   "",
		"invalid": "abc",
		"zero":    "0",
		"neg":     "-1",
	} {
		path := filepath.Join(base, name+".pid")
		if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
			t.Fatalf("write %s pid file: %v", name, err)
		}
		if _, err := readPID(path); err == nil {
			t.Fatalf("expected error for %s pid file", name)
		}
	}
}

func TestProcessChecks(t *testing.T) {
	if processExists(-1) {
		t.Fatal("expected processExists false for invalid pid")
	}
	if !processExists(os.Getpid()) {
		t.Fatal("expected processExists true for current pid")
	}

	cmd := exec.Command("sleep", "2")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	time.Sleep(50 * time.Millisecond)
	if !processCmdlineContains(cmd.Process.Pid, "sleep") {
		t.Fatalf("expected cmdline to contain sleep for pid %d", cmd.Process.Pid)
	}
	if processCmdlineContains(cmd.Process.Pid, "definitely-not-there") {
		t.Fatalf("did not expect unmatched token in cmdline for pid %d", cmd.Process.Pid)
	}
}

func TestTCPListenerPresentInFile(t *testing.T) {
	base := t.TempDir()
	tcpPath := filepath.Join(base, "tcp")
	content := strings.Join([]string{
		"  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode",
		"   0: 0100007F:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000   100        0 1",
		"   1: 0100007F:1F90 00000000:0000 01 00000000:00000000 00:00000000 00000000   100        0 2",
	}, "\n")
	if err := os.WriteFile(tcpPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write tcp file: %v", err)
	}

	if !tcpListenerPresentInFile(tcpPath, 22) {
		t.Fatal("expected listener on port 22")
	}
	if tcpListenerPresentInFile(tcpPath, 8080) {
		t.Fatal("did not expect listener on port 8080")
	}
}

func TestProbeSSHBanner(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("SSH-2.0-dropbear_2022.83\r\n"))
	}()

	ok := probeSSHBanner(ln.Addr().String(), 500*time.Millisecond)
	<-done
	if !ok {
		t.Fatal("expected probeSSHBanner to detect dropbear banner")
	}

	if probeSSHBanner("127.0.0.1:1", 100*time.Millisecond) {
		t.Fatal("expected probeSSHBanner false for unreachable address")
	}
}

func TestErrorStringAndDropbearStatus(t *testing.T) {
	if errorString(nil) != "" {
		t.Fatal("expected empty string for nil error")
	}
	if got := errorString(context.DeadlineExceeded); !strings.Contains(got, "deadline") {
		t.Fatalf("unexpected error string: %q", got)
	}

	status := dropbearStatus()
	if status.Name != "dropbear" {
		t.Fatalf("unexpected dropbear status name: %+v", status)
	}
	if status.Detail == nil {
		t.Fatalf("expected dropbear detail map, got %+v", status)
	}
}

func TestInitServiceManagerInputValidationPaths(t *testing.T) {
	m := NewInitServiceManager()
	if _, err := m.Status(context.Background(), "bad/name"); err == nil {
		t.Fatal("expected status validation error")
	}
	if err := m.Restart(context.Background(), "bad/name"); err == nil {
		t.Fatal("expected restart validation error")
	}
}

