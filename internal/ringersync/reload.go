package ringersync

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"
)

const (
	// ConfXMLPath is the configuration bt_daemon watches: a new mtime makes it
	// restart the firmware processes, the GUI included.
	ConfXMLPath = "/var/tmp/conf.xml"
	// dropbearInit is the SSH server's init script.
	dropbearInit = "/etc/init.d/dropbear"
	// defaultSSHWait is how long after the reload SSH is checked: the reload
	// stops dropbear, and c300x-controller starts it again after 60 s.
	defaultSSHWait = 60 * time.Second
)

// FirmwareReloader reloads the intercom firmware by touching conf.xml, as
// c300x-controller's reloadUi does. On a C100X (2026-11-05) bt_daemon then
// restarts openserver, flexisip, the GUI, bt_vct, bt_av_media, mosquitto and
// dropbear: the bus is down for about 25 s, and the GUI reads the ringer
// state again when it starts.
type FirmwareReloader struct {
	Path    string
	SSHWait time.Duration
	// SSHListening reports whether the SSH server accepts connections.
	SSHListening func() bool
	// StartSSH starts the SSH server again.
	StartSSH func(context.Context) error
}

func NewFirmwareReloader() *FirmwareReloader {
	return &FirmwareReloader{
		Path:         ConfXMLPath,
		SSHWait:      defaultSSHWait,
		SSHListening: sshListening,
		StartSSH: func(ctx context.Context) error {
			return exec.CommandContext(ctx, dropbearInit, "start").Run() // #nosec G204 -- fixed init script path.
		},
	}
}

// Reload touches conf.xml, never creating it, and starts SSH again if the
// reload stopped it. It returns once SSH has been checked.
func (r *FirmwareReloader) Reload(ctx context.Context) error {
	if _, err := os.Stat(r.Path); err != nil {
		return fmt.Errorf("firmware reload: %w", err)
	}

	sshBefore := r.SSHListening != nil && r.SSHListening()

	now := time.Now()
	if err := os.Chtimes(r.Path, now, now); err != nil {
		return fmt.Errorf("firmware reload: touch %s: %w", r.Path, err)
	}

	if !sshBefore || r.StartSSH == nil {
		return nil
	}

	timer := time.NewTimer(r.SSHWait)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return nil
	case <-timer.C:
	}

	if r.SSHListening() {
		return nil
	}

	if err := r.StartSSH(ctx); err != nil {
		return fmt.Errorf("firmware reload: start ssh again: %w", err)
	}

	return nil
}

func sshListening() bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:22", time.Second)
	if err != nil {
		return false
	}

	_ = conn.Close()

	return true
}
