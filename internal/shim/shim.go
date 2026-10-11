// Package shim answers the commands software expects to find on a systemd
// host (sudo, systemctl, timedatectl and systemd-run) from inside a container,
// using the unit state notsystemd records.
//
// The notsystemd binary dispatches to these when it is invoked under one of
// those names, so an image links them to it.
package shim

import (
	"io"
	"os"

	"github.com/unmango/notsystemd/internal/state"
)

// Env is what a shim reads and writes, so specs can run one without touching
// the process.
type Env struct {
	Stdout, Stderr io.Writer
	// StateDir is where the units' state files live.
	StateDir string
	// Signal sends sig to pid; nil means syscall.Kill.
	Signal func(pid int, sig os.Signal) error
}

// Default is the Env for the running process. NOTSYSTEMD_STATE_DIR overrides
// the state directory, since a shim has no flags of its own to take it.
func Default() Env {
	dir := os.Getenv("NOTSYSTEMD_STATE_DIR")
	if dir == "" {
		dir = state.DefaultDir
	}
	return Env{Stdout: os.Stdout, Stderr: os.Stderr, StateDir: dir}
}

func (e Env) signal(pid int, sig os.Signal) error {
	if e.Signal != nil {
		return e.Signal(pid, sig)
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(sig)
}

// Names are the commands notsystemd answers to when invoked under them.
var Names = []string{"sudo", "systemctl", "systemd-run", "timedatectl"}
