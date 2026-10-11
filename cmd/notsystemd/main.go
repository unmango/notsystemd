// Command notsystemd runs a systemd service unit in a container, without
// systemd.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/unmango/notsystemd/internal/notify"
	"github.com/unmango/notsystemd/internal/shim"
	"github.com/unmango/notsystemd/internal/state"
	"github.com/unmango/notsystemd/internal/supervisor"
	"github.com/unmango/notsystemd/internal/unit"
)

const usage = `Usage: notsystemd <command> [flags] [args]

Commands:
  run UNIT_FILE        run a service unit until it exits or notsystemd is signalled
  is-active UNIT       exit 0 if the unit is active, for readiness probes
  show UNIT            print the unit's recorded state as JSON
  notify ASSIGNMENT... send KEY=VALUE assignments to $NOTIFY_SOCKET

  sudo, systemctl, systemd-run, timedatectl
                       stand-ins for those commands, also run when notsystemd
                       is invoked under their names

Run "notsystemd <command> -h" for a command's flags.
`

func main() {
	// Invoked through a link named after a command it stands in for.
	if name := filepath.Base(os.Args[0]); slices.Contains(shim.Names, name) {
		os.Exit(shimCmd(name, os.Args[1:]))
	}
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var code int
	switch cmd {
	case "run":
		code = runCmd(args)
	case "is-active":
		code = isActiveCmd(args)
	case "show":
		code = showCmd(args)
	case "notify":
		code = notifyCmd(args)
	case "sudo", "systemctl", "systemd-run", "timedatectl":
		code = shimCmd(cmd, args)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "notsystemd: unknown command %q\n\n%s", cmd, usage)
		code = 2
	}
	os.Exit(code)
}

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func runCmd(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	stateDir := fs.String("state-dir", state.DefaultDir, "directory for unit state and notify sockets")
	verbose := fs.Bool("verbose", false, "log every command notsystemd runs")
	var dropIns stringList
	fs.Var(&dropIns, "drop-in-dir", "also apply drop-ins from DIR/<unit>.d (repeatable; later wins)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: notsystemd run [flags] UNIT_FILE")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	svc, err := unit.LoadService(fs.Arg(0), dropIns...)
	if err != nil {
		log.Error("loading unit", "err", err)
		return 1
	}

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	reload := make(chan struct{}, 1)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		for sig := range sigs {
			if sig == syscall.SIGHUP {
				select {
				case reload <- struct{}{}:
				default:
				}
				continue
			}
			log.Info("stopping", "unit", svc.Name, "signal", sig)
			stop()
		}
	}()

	s := &supervisor.Supervisor{Service: svc, StateDir: *stateDir, Log: log}
	return s.Run(ctx, reload)
}

func unitName(arg string) string {
	if strings.Contains(arg, ".") {
		return arg
	}
	return arg + ".service"
}

func isActiveCmd(args []string) int {
	fs := flag.NewFlagSet("is-active", flag.ExitOnError)
	stateDir := fs.String("state-dir", state.DefaultDir, "directory for unit state")
	quiet := fs.Bool("quiet", false, "print nothing, only set the exit status")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "Usage: notsystemd is-active [flags] UNIT")
		return 2
	}

	u, err := state.Read(*stateDir, unitName(fs.Arg(0)))
	active := "inactive"
	if err == nil {
		active = u.ActiveState
	} else if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(os.Stderr, err)
		return 4
	}
	if !*quiet {
		fmt.Println(active)
	}
	// Like systemctl, a reloading unit still counts as active.
	if active == "active" || active == "reloading" {
		return 0
	}
	return 3
}

func showCmd(args []string) int {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	stateDir := fs.String("state-dir", state.DefaultDir, "directory for unit state")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "Usage: notsystemd show [flags] UNIT")
		return 2
	}
	u, err := state.Read(*stateDir, unitName(fs.Arg(0)))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 4
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(u)
	return 0
}

func notifyCmd(args []string) int {
	fs := flag.NewFlagSet("notify", flag.ExitOnError)
	ready := fs.Bool("ready", false, "send READY=1")
	status := fs.String("status", "", "send STATUS=TEXT")
	fs.Parse(args)

	assignments := fs.Args()
	if *ready {
		assignments = append(assignments, "READY=1")
	}
	if *status != "" {
		assignments = append(assignments, "STATUS="+*status)
	}
	if len(assignments) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: notsystemd notify [--ready] [--status TEXT] [KEY=VALUE...]")
		return 2
	}
	path := os.Getenv("NOTIFY_SOCKET")
	if path == "" {
		fmt.Fprintln(os.Stderr, "notsystemd: NOTIFY_SOCKET is not set")
		return 1
	}
	if err := notify.Send(path, strings.Join(assignments, "\n")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func shimCmd(name string, args []string) int {
	e := shim.Default()
	switch name {
	case "systemctl":
		return shim.Systemctl(e, args)
	case "timedatectl":
		return shim.Timedatectl(e, args)
	}

	parse := shim.ParseSudo
	if name == "systemd-run" {
		parse = shim.ParseSystemdRun
	}
	plan, err := parse(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		return 1
	}
	return shim.Exec(e, plan)
}
