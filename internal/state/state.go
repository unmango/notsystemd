// Package state records what notsystemd knows about a unit in a JSON file,
// so that probes, and the other containers of a pod sharing the directory,
// can read it without talking to the process running the unit.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// DefaultDir is where notsystemd keeps unit state unless told otherwise.
const DefaultDir = "/run/notsystemd"

// Unit mirrors the systemd properties of the same names.
type Unit struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// ActiveState is activating, active, reloading, deactivating, inactive
	// or failed.
	ActiveState string `json:"activeState"`
	SubState    string `json:"subState"`
	// StatusText is the last STATUS= the service sent.
	StatusText string `json:"statusText,omitempty"`
	MainPID    int    `json:"mainPID,omitempty"`
	// Result is success, or why the unit failed: exit-code, signal,
	// timeout, watchdog or resources.
	Result     string    `json:"result"`
	ExitStatus int       `json:"exitStatus"`
	Updated    time.Time `json:"updated"`
}

// Path is where the state of the unit named name lives under dir.
func Path(dir, name string) string {
	return filepath.Join(dir, name+".json")
}

// Write records u under dir, replacing the file atomically so a reader never
// sees half of it.
func Write(dir string, u Unit) error {
	u.Updated = time.Now().UTC()
	data, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+u.Name+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), Path(dir, u.Name))
}

// Read returns the recorded state of the unit named name.
func Read(dir, name string) (Unit, error) {
	var u Unit
	data, err := os.ReadFile(Path(dir, name))
	if err != nil {
		return u, err
	}
	err = json.Unmarshal(data, &u)
	return u, err
}
