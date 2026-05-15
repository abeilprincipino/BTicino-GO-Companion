package system

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	shutdownBinaryPath = "/sbin/shutdown"
	initdBaseDir       = "/etc/init.d"
	dropbearPIDPath    = "/var/run/dropbear.pid"
)

var serviceNamePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

type ServiceStatus struct {
	Name    string         `json:"name"`
	Running bool           `json:"running"`
	Output  string         `json:"output,omitempty"`
	Detail  map[string]any `json:"detail,omitempty"`
}

type ServiceManager interface {
	RebootHost(ctx context.Context) error
	Status(ctx context.Context, serviceName string) (ServiceStatus, error)
	Restart(ctx context.Context, serviceName string) error
}

type InitServiceManager struct{}

func NewInitServiceManager() *InitServiceManager {
	return &InitServiceManager{}
}

func (m *InitServiceManager) RebootHost(ctx context.Context) error {
	output, err := runExecCommand(ctx, shutdownBinaryPath, "-r", "now")
	if err != nil {
		if output != "" {
			return fmt.Errorf("reboot command failed: %w output=%s", err, output)
		}
		return fmt.Errorf("reboot command failed: %w", err)
	}
	return nil
}

func (m *InitServiceManager) Status(ctx context.Context, serviceName string) (ServiceStatus, error) {
	name, err := normalizeServiceName(serviceName)
	if err != nil {
		return ServiceStatus{}, err
	}
	if name == "dropbear" {
		return dropbearStatus(), nil
	}

	output, runErr := runExecCommand(ctx, initScriptPath(name), "status")
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return ServiceStatus{Name: name, Running: false, Output: output}, nil
		}
		return ServiceStatus{}, fmt.Errorf("service status failed: %w", runErr)
	}
	return ServiceStatus{Name: name, Running: true, Output: output}, nil
}

func (m *InitServiceManager) Restart(ctx context.Context, serviceName string) error {
	name, err := normalizeServiceName(serviceName)
	if err != nil {
		return err
	}
	output, runErr := runExecCommand(ctx, initScriptPath(name), "restart")
	if runErr != nil {
		if output != "" {
			return fmt.Errorf("service restart failed: %w output=%s", runErr, output)
		}
		return fmt.Errorf("service restart failed: %w", runErr)
	}
	return nil
}

func initScriptPath(serviceName string) string {
	return filepath.Join(initdBaseDir, serviceName)
}

func normalizeServiceName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", errors.New("service name is required")
	}
	if !serviceNamePattern.MatchString(name) {
		return "", errors.New("service name is invalid")
	}
	return strings.ToLower(name), nil
}

func runExecCommand(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func dropbearStatus() ServiceStatus {
	pid, pidErr := readPID(dropbearPIDPath)
	pidValid := pid > 0
	processAlive := false
	cmdlineContains := false
	if pidValid {
		processAlive = processExists(pid)
		cmdlineContains = processCmdlineContains(pid, "dropbear")
	}

	listener := tcpListenerPresent(22)
	banner := probeSSHBanner("127.0.0.1:22", 1200*time.Millisecond)
	running := listener && (processAlive || banner)

	detail := map[string]any{
		"pidfile":          dropbearPIDPath,
		"pid":              pid,
		"pid_read_error":   errorString(pidErr),
		"pid_alive":        processAlive,
		"cmdline_dropbear": cmdlineContains,
		"tcp_22_listening": listener,
		"ssh_banner_ok":    banner,
	}

	return ServiceStatus{
		Name:    "dropbear",
		Running: running,
		Detail:  detail,
	}
}

func readPID(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return 0, errors.New("pid file is empty")
	}
	pid, err := strconv.Atoi(value)
	if err != nil {
		return 0, err
	}
	if pid <= 0 {
		return 0, errors.New("pid is not positive")
	}
	return pid, nil
}

func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscallKillZero(pid)
	return err == nil
}

func syscallKillZero(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(syscall.Signal(0))
}

func processCmdlineContains(pid int, needle string) bool {
	if pid <= 0 {
		return false
	}
	path := fmt.Sprintf("/proc/%d/cmdline", pid)
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	normalized := strings.ToLower(strings.ReplaceAll(string(raw), "\x00", " "))
	return strings.Contains(normalized, strings.ToLower(strings.TrimSpace(needle)))
}

func tcpListenerPresent(port int) bool {
	return tcpListenerPresentInFile("/proc/net/tcp", port) || tcpListenerPresentInFile("/proc/net/tcp6", port)
}

func tcpListenerPresentInFile(path string, port int) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	targetPort := strings.ToUpper(fmt.Sprintf("%04X", port))
	scanner := bufio.NewScanner(f)
	first := true
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if first {
			first = false
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		localAddr := fields[1]
		state := fields[3]
		if state != "0A" {
			continue
		}
		idx := strings.LastIndex(localAddr, ":")
		if idx == -1 {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(localAddr[idx+1:]), targetPort) {
			return true
		}
	}
	return false
}

func probeSSHBanner(addr string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	_ = conn.SetWriteDeadline(time.Now().Add(timeout))

	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false
	}
	normalized := strings.ToLower(strings.TrimSpace(line))
	return strings.Contains(normalized, "ssh-") && strings.Contains(normalized, "dropbear")
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
