package shim

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/unmango/notsystemd/internal/state"
	"github.com/unmango/notsystemd/internal/unit"
)

// systemctl's documented exit codes for is-active and status.
const (
	exitNotActive = 3
	exitNoUnit    = 4
)

type systemctlOpts struct {
	quiet, now, value bool
	props             []string
	signal            syscall.Signal
}

// Systemctl answers the systemctl verbs from the units' recorded state.
//
// Verbs that change a unit act on the notsystemd process running it: restart
// and stop send it SIGTERM, which ends its container, and Kubernetes starts
// the container again. Which units run, and whether they are enabled, is the
// pod's business, so enable, disable, mask and the like succeed without doing
// anything, and so does starting a unit that is not running.
func Systemctl(e Env, args []string) int {
	o := systemctlOpts{signal: syscall.SIGTERM}
	var words []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		// Flags that take a value, as "--flag=value", "--flag value" or "-f value".
		flagValue := func(long, short string) (string, bool) {
			if v, ok := strings.CutPrefix(a, long+"="); ok {
				return v, true
			}
			if (a == long || a == short) && i+1 < len(args) {
				i++
				return args[i], true
			}
			return "", false
		}
		if v, ok := flagValue("--property", "-p"); ok {
			o.props = append(o.props, strings.Split(v, ",")...)
			continue
		}
		if v, ok := flagValue("--signal", "-s"); ok {
			sig, err := unit.ParseSignal(v)
			if err != nil {
				fmt.Fprintf(e.Stderr, "systemctl: %v\n", err)
				return 1
			}
			o.signal = sig
			continue
		}
		if _, ok := flagValue("--kill-whom", ""); ok {
			continue
		}
		if _, ok := flagValue("--kill-who", ""); ok {
			continue
		}
		switch a {
		case "-q", "--quiet":
			o.quiet = true
		case "--now":
			o.now = true
		case "--value":
			o.value = true
		default:
			if !strings.HasPrefix(a, "-") {
				words = append(words, a)
			}
			// Everything else (--wait, --no-block, --no-pager, --system,
			// --plain, --full...) changes nothing here.
		}
	}
	if len(words) == 0 {
		fmt.Fprintln(e.Stderr, "systemctl: no verb given")
		return 1
	}
	verb, units := words[0], words[1:]
	for i, u := range units {
		units[i] = unitName(u)
	}

	switch verb {
	case "is-system-running":
		if !o.quiet {
			fmt.Fprintln(e.Stdout, "running")
		}
		return 0
	case "is-active", "is-failed", "is-enabled":
		return e.isState(verb, units, o.quiet)
	case "show":
		return e.show(units, o)
	case "status":
		return e.status(units)
	case "start", "enable", "disable", "mask", "unmask", "daemon-reload",
		"daemon-reexec", "reset-failed", "preset", "revert":
		if o.now && (verb == "disable" || verb == "mask") {
			fmt.Fprintf(e.Stderr, "systemctl: ignoring %s --now: the pod decides which units run\n", verb)
		}
		return 0
	case "stop", "restart", "try-restart", "condrestart", "reload-or-restart", "try-reload-or-restart":
		return e.signalSupervisor(units, syscall.SIGTERM)
	case "reload":
		return e.signalSupervisor(units, syscall.SIGHUP)
	case "kill":
		return e.killMain(units, o.signal)
	default:
		fmt.Fprintf(e.Stderr, "systemctl: %s is not supported by notsystemd\n", verb)
		return 1
	}
}

func unitName(arg string) string {
	if strings.Contains(arg, ".") {
		return arg
	}
	return arg + ".service"
}

// read returns a unit's state, and false when nothing records it.
func (e Env) read(name string) (state.Unit, bool, error) {
	u, err := state.Read(e.StateDir, name)
	if errors.Is(err, os.ErrNotExist) {
		return state.Unit{Name: name, ActiveState: "inactive", SubState: "dead"}, false, nil
	}
	return u, err == nil, err
}

func (e Env) isState(verb string, units []string, quiet bool) int {
	if len(units) == 0 {
		fmt.Fprintf(e.Stderr, "systemctl: %s needs a unit\n", verb)
		return 1
	}
	code := 1
	if verb == "is-active" {
		code = exitNotActive
	}
	for _, name := range units {
		u, found, err := e.read(name)
		if err != nil {
			fmt.Fprintf(e.Stderr, "systemctl: %v\n", err)
			return exitNoUnit
		}
		var answer string
		var yes bool
		switch verb {
		case "is-active":
			answer = u.ActiveState
			yes = answer == "active" || answer == "reloading"
		case "is-failed":
			answer = u.ActiveState
			yes = answer == "failed"
		case "is-enabled":
			answer, yes = "disabled", found
			if found {
				answer = "enabled"
			}
		}
		if !quiet {
			fmt.Fprintln(e.Stdout, answer)
		}
		if yes {
			code = 0
		}
	}
	return code
}

func properties(u state.Unit, found bool) [][2]string {
	load, fileState := "not-found", ""
	if found {
		load, fileState = "loaded", "enabled"
	}
	return [][2]string{
		{"Id", u.Name},
		{"Names", u.Name},
		{"Description", u.Description},
		{"LoadState", load},
		{"ActiveState", u.ActiveState},
		{"SubState", u.SubState},
		{"UnitFileState", fileState},
		{"StatusText", u.StatusText},
		{"MainPID", strconv.Itoa(u.MainPID)},
		{"ExecMainPID", strconv.Itoa(u.MainPID)},
		{"ExecMainStatus", strconv.Itoa(u.ExitStatus)},
		{"Result", u.Result},
	}
}

func (e Env) show(units []string, o systemctlOpts) int {
	var sets [][][2]string
	if len(units) == 0 {
		sets = append(sets, [][2]string{{"SystemState", "running"}, {"Version", "notsystemd"}})
	}
	for _, name := range units {
		u, found, err := e.read(name)
		if err != nil {
			fmt.Fprintf(e.Stderr, "systemctl: %v\n", err)
			return 1
		}
		sets = append(sets, properties(u, found))
	}

	for i, props := range sets {
		if i > 0 {
			fmt.Fprintln(e.Stdout)
		}
		if len(o.props) > 0 {
			var picked [][2]string
			for _, want := range o.props {
				i := slices.IndexFunc(props, func(p [2]string) bool { return p[0] == want })
				if i >= 0 {
					picked = append(picked, props[i])
				}
			}
			props = picked
		}
		for _, p := range props {
			if o.value {
				fmt.Fprintln(e.Stdout, p[1])
			} else {
				fmt.Fprintf(e.Stdout, "%s=%s\n", p[0], p[1])
			}
		}
	}
	return 0
}

func (e Env) status(units []string) int {
	code := 0
	for i, name := range units {
		u, found, err := e.read(name)
		if err != nil {
			fmt.Fprintf(e.Stderr, "systemctl: %v\n", err)
			return 1
		}
		if !found {
			fmt.Fprintf(e.Stderr, "Unit %s could not be found.\n", name)
			code = exitNoUnit
			continue
		}
		if i > 0 {
			fmt.Fprintln(e.Stdout)
		}
		fmt.Fprintf(e.Stdout, "● %s - %s\n", u.Name, u.Description)
		fmt.Fprintln(e.Stdout, "     Loaded: loaded")
		fmt.Fprintf(e.Stdout, "     Active: %s (%s)\n", u.ActiveState, u.SubState)
		if u.MainPID != 0 {
			fmt.Fprintf(e.Stdout, "   Main PID: %d\n", u.MainPID)
		}
		if u.StatusText != "" {
			fmt.Fprintf(e.Stdout, "     Status: %q\n", u.StatusText)
		}
		if u.ActiveState != "active" && u.ActiveState != "reloading" && code == 0 {
			code = exitNotActive
		}
	}
	return code
}

func running(u state.Unit) bool {
	return u.ActiveState != "inactive" && u.ActiveState != "failed"
}

func (e Env) signalSupervisor(units []string, sig syscall.Signal) int {
	code := 0
	for _, name := range units {
		u, found, err := e.read(name)
		if err != nil {
			fmt.Fprintf(e.Stderr, "systemctl: %v\n", err)
			code = 1
			continue
		}
		if !found || !running(u) || u.SupervisorPID == 0 {
			continue
		}
		if err := e.signal(u.SupervisorPID, sig); err != nil {
			fmt.Fprintf(e.Stderr, "systemctl: signalling %s: %v\n", name, err)
			code = 1
		}
	}
	return code
}

func (e Env) killMain(units []string, sig syscall.Signal) int {
	code := 0
	for _, name := range units {
		u, found, err := e.read(name)
		if err != nil {
			fmt.Fprintf(e.Stderr, "systemctl: %v\n", err)
			code = 1
			continue
		}
		if !found || u.MainPID == 0 {
			continue
		}
		if err := e.signal(u.MainPID, sig); err != nil {
			fmt.Fprintf(e.Stderr, "systemctl: killing %s: %v\n", name, err)
			code = 1
		}
	}
	return code
}
