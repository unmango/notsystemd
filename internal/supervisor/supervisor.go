// Package supervisor runs one service unit the way systemd would, inside a
// container: the ExecStartPre= to ExecStopPost= lifecycle, the unit's
// user, environment and directories, readiness over $NOTIFY_SOCKET, the
// watchdog, and stop timeouts.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/unmango/notsystemd/internal/notify"
	"github.com/unmango/notsystemd/internal/state"
	"github.com/unmango/notsystemd/internal/unit"
)

// prSetChildSubreaper is PR_SET_CHILD_SUBREAPER from <linux/prctl.h>.
const prSetChildSubreaper = 36

// defaultPath is the PATH systemd gives services.
const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// Supervisor runs one service.
type Supervisor struct {
	Service *unit.Service
	// StateDir is where the unit's state file and notify socket live.
	StateDir string
	Log      *slog.Logger
}

type run struct {
	*Supervisor
	r     *reaper
	st    state.Unit
	env   map[string]string
	creds *syscall.Credential
	home  string
	dir   string

	notify   *notify.Listener
	messages <-chan notify.Message

	mainPID  int
	mainExit <-chan syscall.WaitStatus
	status   syscall.WaitStatus
	exited   bool
}

// Run starts the service, keeps it running until it exits or ctx is
// cancelled, then stops it. Each receive on reload runs ExecReload=. It
// returns the exit code notsystemd should exit with.
func (s *Supervisor) Run(ctx context.Context, reload <-chan struct{}) int {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	if s.StateDir == "" {
		s.StateDir = state.DefaultDir
	}

	ru := &run{
		Supervisor: s,
		r:          newReaper(),
		st: state.Unit{
			Name:        s.Service.Name,
			Description: s.Service.Description,
			ActiveState: "activating",
			SubState:    "start-pre",
			Result:      "success",
		},
	}
	defer ru.r.stop()
	ru.write()

	// Outside PID 1, become a subreaper so a daemon that forks away from its
	// parent stays our child and its exit status reaches the reaper.
	if os.Getpid() != 1 {
		if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); errno != 0 {
			s.Log.Warn("could not become a subreaper", "err", errno)
		}
	}

	if err := ru.prepare(); err != nil {
		s.Log.Error("preparing to start", "unit", s.Service.Name, "err", err)
		ru.st.Result = "resources"
		return ru.finish(false)
	}
	defer ru.cleanup()

	started := ru.start(ctx)
	if started {
		ru.supervise(ctx, reload)
	}
	return ru.finish(started)
}

func (ru *run) write() {
	if err := state.Write(ru.StateDir, ru.st); err != nil {
		ru.Log.Warn("writing unit state", "err", err)
	}
}

func (ru *run) setState(active, sub string) {
	ru.st.ActiveState, ru.st.SubState = active, sub
	ru.write()
}

// prepare resolves the unit's user, environment and directories, and opens
// its notify socket.
func (ru *run) prepare() error {
	svc := ru.Service

	if svc.UMask != nil {
		syscall.Umask(int(*svc.UMask))
	} else {
		syscall.Umask(0o022)
	}

	ru.env = map[string]string{"PATH": defaultPath}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			ru.env[k] = v
		}
	}

	if svc.User != "" || svc.Group != "" {
		creds, u, err := lookupCredentials(svc.User, svc.Group)
		if err != nil {
			return err
		}
		ru.creds = creds
		if u != nil {
			ru.home = u.HomeDir
			ru.env["USER"] = u.Username
			ru.env["LOGNAME"] = u.Username
			ru.env["HOME"] = u.HomeDir
		}
	}

	for _, kv := range svc.Environment {
		k, v, _ := strings.Cut(kv, "=")
		ru.env[k] = v
	}
	for _, ef := range svc.EnvironmentFiles {
		assignments, err := unit.ReadEnvironmentFile(ef.Path)
		if err != nil {
			if ef.Optional && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("EnvironmentFile=%s: %w", ef.Path, err)
		}
		for _, kv := range assignments {
			k, v, _ := strings.Cut(kv, "=")
			ru.env[k] = v
		}
	}

	for key, dirs := range svc.Directories {
		for _, d := range dirs {
			if err := os.MkdirAll(d, 0o755); err != nil {
				return fmt.Errorf("%s=%s: %w", key, d, err)
			}
			if err := os.Chmod(d, os.FileMode(svc.DirectoryModes[key])); err != nil {
				return fmt.Errorf("%s=%s: %w", key, d, err)
			}
			if ru.creds != nil && key != "ConfigurationDirectory" {
				if err := os.Lchown(d, int(ru.creds.Uid), int(ru.creds.Gid)); err != nil {
					return fmt.Errorf("%s=%s: %w", key, d, err)
				}
			}
		}
		envName := strings.ToUpper(strings.TrimSuffix(key, "Directory")) + "_DIRECTORY"
		if key == "ConfigurationDirectory" {
			envName = "CONFIGURATION_DIRECTORY"
		}
		ru.env[envName] = strings.Join(dirs, ":")
	}

	ru.dir = "/"
	if wd := svc.WorkingDirectory; wd != "" {
		if wd == "~" {
			wd = ru.home
		}
		if _, err := os.Stat(wd); err == nil {
			ru.dir = wd
		} else if !svc.WorkingDirectoryOptional {
			return fmt.Errorf("WorkingDirectory=%s: %w", wd, err)
		}
	}

	if svc.Type == "notify" || svc.Watchdog > 0 {
		l, err := notify.Listen(filepath.Join(ru.StateDir, svc.Name+".notify"))
		if err != nil {
			return fmt.Errorf("notify socket: %w", err)
		}
		if ru.creds != nil {
			// The socket is world-writable, but its directory has to be
			// reachable by the service's user too.
			os.Chmod(ru.StateDir, 0o755)
		}
		ru.notify = l
		ru.messages = l.Messages()
		ru.env["NOTIFY_SOCKET"] = l.Path
	}
	if svc.Watchdog > 0 {
		ru.env["WATCHDOG_USEC"] = strconv.FormatInt(svc.Watchdog.Microseconds(), 10)
	}
	return nil
}

func (ru *run) cleanup() {
	if ru.notify != nil {
		ru.notify.Close()
	}
	for _, d := range ru.Service.Directories["RuntimeDirectory"] {
		os.RemoveAll(d)
	}
}

// start runs everything up to the service being ready: ExecStartPre=,
// ExecStart= and ExecStartPost=. It reports whether the service started.
func (ru *run) start(ctx context.Context) bool {
	svc := ru.Service
	startCtx := ctx
	if svc.TimeoutStart > 0 {
		var cancel context.CancelFunc
		startCtx, cancel = context.WithTimeout(ctx, svc.TimeoutStart)
		defer cancel()
	}

	if !ru.runAll(startCtx, svc.ExecStartPre, "ExecStartPre") {
		return false
	}

	ru.setState("activating", "start")
	switch svc.Type {
	case "oneshot":
		if !ru.runAll(startCtx, svc.ExecStart, "ExecStart") {
			return false
		}
	case "forking":
		if !ru.startForking(startCtx) {
			return false
		}
	default:
		pid, err := ru.spawn(svc.ExecStart[0])
		if err != nil {
			ru.Log.Error("starting", "unit", svc.Name, "err", err)
			ru.st.Result = "exit-code"
			return false
		}
		ru.setMain(pid)
		if svc.Type == "notify" && !ru.awaitReady(startCtx) {
			return false
		}
	}

	ru.setState("activating", "start-post")
	if !ru.runAll(startCtx, svc.ExecStartPost, "ExecStartPost") {
		return false
	}

	if svc.Type == "oneshot" && !svc.RemainAfterExit {
		ru.setState("deactivating", "stop")
	} else if svc.Type == "oneshot" {
		ru.setState("active", "exited")
	} else {
		ru.setState("active", "running")
	}
	return true
}

func (ru *run) setMain(pid int) {
	ru.mainPID = pid
	ru.mainExit = ru.r.watchAny(pid)
	ru.st.MainPID = pid
	ru.env["MAINPID"] = strconv.Itoa(pid)
}

func (ru *run) startForking(ctx context.Context) bool {
	svc := ru.Service
	pid, err := ru.spawn(svc.ExecStart[0])
	if err != nil {
		ru.Log.Error("starting", "unit", svc.Name, "err", err)
		ru.st.Result = "exit-code"
		return false
	}
	select {
	case ws := <-ru.r.watch(pid):
		if !ws.Exited() || ws.ExitStatus() != 0 {
			ru.Log.Error("forking ExecStart= failed", "unit", svc.Name, "status", describe(ws))
			ru.st.Result, ru.st.ExitStatus = result(ws), exitCode(ws)
			return false
		}
	case <-ctx.Done():
		ru.killGroup(pid, syscall.SIGKILL)
		ru.st.Result = "timeout"
		return false
	}

	if svc.PIDFile == "" {
		// Without a PID file the daemon is whatever is left running; it
		// stays our child as long as we are PID 1 or a subreaper.
		idle := ru.r.whenIdle()
		ch := make(chan syscall.WaitStatus, 1)
		go func() { <-idle; ch <- 0 }()
		ru.mainExit = ch
		return true
	}

	for {
		data, err := os.ReadFile(svc.PIDFile)
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
				ru.setMain(pid)
				return true
			}
		}
		select {
		case <-ctx.Done():
			ru.Log.Error("PIDFile= never appeared", "unit", svc.Name, "path", svc.PIDFile)
			ru.st.Result = "timeout"
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// awaitReady waits for READY=1 from a Type=notify service.
func (ru *run) awaitReady(ctx context.Context) bool {
	for {
		select {
		case ws := <-ru.mainExit:
			ru.mainExited(ws)
			ru.Log.Error("exited before it was ready", "unit", ru.Service.Name, "status", describe(ws))
			return false
		case <-ctx.Done():
			ru.Log.Error("never sent READY=1", "unit", ru.Service.Name)
			ru.st.Result = "timeout"
			return false
		case m, ok := <-ru.messages:
			if !ok {
				ru.messages = nil
				continue
			}
			ru.handle(m)
			if m["READY"] == "1" {
				return true
			}
		}
	}
}

// handle applies a notification's effect on the recorded state. It reports
// whether the message asked for a watchdog failure.
func (ru *run) handle(m notify.Message) bool {
	if v, ok := m["STATUS"]; ok {
		ru.st.StatusText = v
	}
	if v, ok := m["MAINPID"]; ok {
		if pid, err := strconv.Atoi(v); err == nil && pid > 0 && pid != ru.mainPID {
			ru.setMain(pid)
		}
	}
	switch {
	case m["RELOADING"] == "1":
		ru.st.ActiveState, ru.st.SubState = "reloading", "reload"
	case m["STOPPING"] == "1":
		ru.st.ActiveState, ru.st.SubState = "deactivating", "stop"
	case m["READY"] == "1" && ru.st.ActiveState == "reloading":
		ru.st.ActiveState, ru.st.SubState = "active", "running"
	}
	ru.write()
	return m["WATCHDOG"] == "trigger"
}

// supervise waits for the running service to exit or be told to stop.
func (ru *run) supervise(ctx context.Context, reload <-chan struct{}) {
	svc := ru.Service
	if svc.Type == "oneshot" && !svc.RemainAfterExit {
		return
	}

	var watchdog <-chan time.Time
	var timer *time.Timer
	resetWatchdog := func(d time.Duration) {
		if timer != nil {
			timer.Stop()
		}
		if d > 0 {
			timer = time.NewTimer(d)
			watchdog = timer.C
		}
	}
	interval := svc.Watchdog
	resetWatchdog(interval)

	for {
		select {
		case <-ctx.Done():
			return
		case ws := <-ru.mainExit:
			ru.mainExited(ws)
			return
		case <-reload:
			ru.reload(ctx)
		case <-watchdog:
			ru.Log.Error("watchdog timeout", "unit", svc.Name, "interval", interval)
			ru.watchdogFailed()
			return
		case m, ok := <-ru.messages:
			if !ok {
				ru.messages = nil
				continue
			}
			if ru.handle(m) {
				ru.Log.Error("service triggered its watchdog", "unit", svc.Name)
				ru.watchdogFailed()
				return
			}
			if v, ok := m["WATCHDOG_USEC"]; ok {
				if us, err := strconv.ParseInt(v, 10, 64); err == nil {
					interval = time.Duration(us) * time.Microsecond
					resetWatchdog(interval)
				}
			}
			if m["WATCHDOG"] == "1" {
				resetWatchdog(interval)
			}
		}
	}
}

func (ru *run) watchdogFailed() {
	ru.st.Result = "watchdog"
	if ru.mainPID > 0 {
		syscall.Kill(ru.mainPID, ru.Service.WatchdogSignal)
	}
}

func (ru *run) reload(ctx context.Context) {
	svc := ru.Service
	if len(svc.ExecReload) == 0 {
		ru.Log.Warn("reload requested, but the unit has no ExecReload=", "unit", svc.Name)
		return
	}
	ru.setState("reloading", "reload")
	reloadCtx := ctx
	if svc.TimeoutStart > 0 {
		var cancel context.CancelFunc
		reloadCtx, cancel = context.WithTimeout(ctx, svc.TimeoutStart)
		defer cancel()
	}
	saved := ru.st.Result
	if !ru.runAll(reloadCtx, svc.ExecReload, "ExecReload") {
		ru.Log.Warn("reload failed", "unit", svc.Name)
	}
	ru.st.Result = saved
	ru.setState("active", "running")
}

func (ru *run) mainExited(ws syscall.WaitStatus) {
	ru.status, ru.exited = ws, true
	ru.st.ExitStatus = exitCode(ws)
	if ru.Service.Type == "forking" && ru.Service.PIDFile == "" {
		return
	}
	ru.Log.Info("main process exited", "unit", ru.Service.Name, "pid", ru.mainPID, "status", describe(ws))
	if ru.st.Result == "success" && !clean(ws) {
		ru.st.Result = result(ws)
	}
}

// finish stops whatever is still running and returns the exit code.
func (ru *run) finish(started bool) int {
	svc := ru.Service
	stopCtx, cancel := context.WithCancel(context.Background())
	if svc.TimeoutStop > 0 {
		stopCtx, cancel = context.WithTimeout(stopCtx, svc.TimeoutStop)
	}
	defer cancel()

	if ru.st.ActiveState != "deactivating" {
		ru.setState("deactivating", "stop")
	}

	if started && len(svc.ExecStop) > 0 {
		ru.runAll(stopCtx, svc.ExecStop, "ExecStop")
	}
	ru.terminate(stopCtx)

	ru.setState("deactivating", "stop-post")
	ru.env["SERVICE_RESULT"] = ru.st.Result
	if ru.exited {
		if ru.status.Signaled() {
			ru.env["EXIT_CODE"] = "killed"
			ru.env["EXIT_STATUS"] = strings.TrimPrefix(unix(ru.status.Signal()), "SIG")
		} else {
			ru.env["EXIT_CODE"] = "exited"
			ru.env["EXIT_STATUS"] = strconv.Itoa(ru.status.ExitStatus())
		}
	}
	postCtx, cancelPost := context.WithCancel(context.Background())
	if svc.TimeoutStop > 0 {
		postCtx, cancelPost = context.WithTimeout(postCtx, svc.TimeoutStop)
	}
	defer cancelPost()
	saved := ru.st.Result
	ru.runAll(postCtx, svc.ExecStopPost, "ExecStopPost")
	ru.st.Result = saved

	ru.st.MainPID = 0
	if ru.st.Result == "success" {
		ru.setState("inactive", "dead")
		return 0
	}
	ru.setState("failed", "failed")
	if ru.st.ExitStatus != 0 {
		return ru.st.ExitStatus
	}
	return 1
}

// terminate signals what is left of the service per KillMode= and waits for
// it, escalating to SIGKILL when ctx expires.
func (ru *run) terminate(ctx context.Context) {
	svc := ru.Service
	pid1 := os.Getpid() == 1

	if ru.mainPID > 0 && !ru.exited && svc.KillMode != "none" {
		if svc.KillMode == "control-group" {
			ru.killGroup(ru.mainPID, svc.KillSignal)
			if pid1 {
				syscall.Kill(-1, svc.KillSignal)
			}
		} else {
			syscall.Kill(ru.mainPID, svc.KillSignal)
		}
		select {
		case ws := <-ru.mainExit:
			ru.mainExited(ws)
		case <-ctx.Done():
			ru.Log.Warn("stop timed out, killing", "unit", svc.Name, "pid", ru.mainPID)
			ru.killGroup(ru.mainPID, syscall.SIGKILL)
			if pid1 && svc.KillMode != "process" {
				syscall.Kill(-1, syscall.SIGKILL)
			}
			ws := <-ru.mainExit
			ru.mainExited(ws)
			ru.st.Result = "timeout"
		}
	}

	// Anything else still running belongs to the service, which is the
	// whole container when notsystemd is PID 1.
	if pid1 && (svc.KillMode == "control-group" || svc.KillMode == "mixed") {
		if svc.KillMode == "control-group" {
			syscall.Kill(-1, svc.KillSignal)
		}
		select {
		case <-ru.r.whenIdle():
		case <-ctx.Done():
			syscall.Kill(-1, syscall.SIGKILL)
			<-ru.r.whenIdle()
		}
	}
}

// runAll runs commands one after another, stopping at the first failure. It
// reports whether they all succeeded, recording the failure otherwise.
func (ru *run) runAll(ctx context.Context, commands []unit.Command, directive string) bool {
	for _, c := range commands {
		pid, err := ru.spawn(c)
		if err != nil {
			ru.Log.Error("starting", "unit", ru.Service.Name, "directive", directive, "command", c.String(), "err", err)
			if c.IgnoreFailure {
				continue
			}
			ru.st.Result = "exit-code"
			return false
		}

		var ws syscall.WaitStatus
		select {
		case ws = <-ru.r.watch(pid):
		case <-ctx.Done():
			ru.killGroup(pid, syscall.SIGTERM)
			select {
			case ws = <-ru.r.watch(pid):
			case <-time.After(5 * time.Second):
				ru.killGroup(pid, syscall.SIGKILL)
			}
			ru.Log.Error("timed out", "unit", ru.Service.Name, "directive", directive, "command", c.String())
			if !c.IgnoreFailure {
				ru.st.Result = "timeout"
				return false
			}
			continue
		}

		if ws.Exited() && ws.ExitStatus() == 0 || c.IgnoreFailure {
			continue
		}
		ru.Log.Error("failed", "unit", ru.Service.Name, "directive", directive, "command", c.String(), "status", describe(ws))
		ru.st.Result, ru.st.ExitStatus = result(ws), exitCode(ws)
		return false
	}
	return true
}

// spawn starts a command as the unit would, without waiting for it.
func (ru *run) spawn(c unit.Command) (int, error) {
	env := ru.env
	path, argv := c.Argv(env)
	if path == "" {
		return 0, errors.New("empty command")
	}
	resolved, err := lookPath(path, env["PATH"])
	if err != nil {
		return 0, err
	}

	envList := make([]string, 0, len(env))
	for k, v := range env {
		envList = append(envList, k+"="+v)
	}

	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return 0, err
	}
	defer devnull.Close()

	attr := &syscall.SysProcAttr{Setpgid: true}
	if ru.creds != nil && !c.Privileged {
		attr.Credential = ru.creds
	}
	ru.Log.Debug("exec", "unit", ru.Service.Name, "argv", argv)
	p, err := os.StartProcess(resolved, argv, &os.ProcAttr{
		Dir:   ru.dir,
		Env:   envList,
		Files: []*os.File{devnull, os.Stdout, os.Stderr},
		Sys:   attr,
	})
	if err != nil {
		return 0, err
	}
	pid := p.Pid
	// The reaper collects the status; the handle only holds a pidfd open.
	p.Release()
	return pid, nil
}

// killGroup signals the process group spawn put pid in, falling back to pid
// alone if it has left it.
func (ru *run) killGroup(pid int, sig syscall.Signal) {
	if err := syscall.Kill(-pid, sig); err != nil {
		syscall.Kill(pid, sig)
	}
}

// lookPath finds an executable the way systemd does for a command that is
// not an absolute path: in the directories of PATH.
func lookPath(name, path string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	for _, dir := range filepath.SplitList(path) {
		candidate := filepath.Join(dir, name)
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s: not found in PATH", name)
}

func lookupCredentials(userName, groupName string) (*syscall.Credential, *user.User, error) {
	creds := &syscall.Credential{}
	var u *user.User
	if userName != "" {
		var err error
		u, err = user.Lookup(userName)
		if err != nil {
			if u, err = user.LookupId(userName); err != nil {
				return nil, nil, fmt.Errorf("User=%s: %w", userName, err)
			}
		}
		uid, _ := strconv.ParseUint(u.Uid, 10, 32)
		gid, _ := strconv.ParseUint(u.Gid, 10, 32)
		creds.Uid, creds.Gid = uint32(uid), uint32(gid)
		if ids, err := u.GroupIds(); err == nil {
			for _, id := range ids {
				if g, err := strconv.ParseUint(id, 10, 32); err == nil {
					creds.Groups = append(creds.Groups, uint32(g))
				}
			}
		}
	} else {
		creds.Uid = uint32(os.Getuid())
	}
	if groupName != "" {
		g, err := user.LookupGroup(groupName)
		if err != nil {
			if g, err = user.LookupGroupId(groupName); err != nil {
				return nil, nil, fmt.Errorf("Group=%s: %w", groupName, err)
			}
		}
		gid, _ := strconv.ParseUint(g.Gid, 10, 32)
		creds.Gid = uint32(gid)
	}
	return creds, u, nil
}

// clean reports whether systemd would count an exit as successful: exit
// status 0, or death by one of the signals it treats as a clean stop.
func clean(ws syscall.WaitStatus) bool {
	if ws.Exited() {
		return ws.ExitStatus() == 0
	}
	switch ws.Signal() {
	case syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGPIPE:
		return true
	}
	return false
}

func result(ws syscall.WaitStatus) string {
	if ws.Signaled() {
		if ws.CoreDump() {
			return "core-dump"
		}
		return "signal"
	}
	return "exit-code"
}

func exitCode(ws syscall.WaitStatus) int {
	if ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ws.ExitStatus()
}

func describe(ws syscall.WaitStatus) string {
	if ws.Signaled() {
		return "killed by " + unix(ws.Signal())
	}
	return "exit status " + strconv.Itoa(ws.ExitStatus())
}

func unix(sig syscall.Signal) string {
	for name, s := range map[string]syscall.Signal{
		"SIGHUP": syscall.SIGHUP, "SIGINT": syscall.SIGINT, "SIGQUIT": syscall.SIGQUIT,
		"SIGABRT": syscall.SIGABRT, "SIGKILL": syscall.SIGKILL, "SIGSEGV": syscall.SIGSEGV,
		"SIGPIPE": syscall.SIGPIPE, "SIGTERM": syscall.SIGTERM, "SIGUSR1": syscall.SIGUSR1,
		"SIGUSR2": syscall.SIGUSR2, "SIGBUS": syscall.SIGBUS,
	} {
		if s == sig {
			return name
		}
	}
	return "signal " + strconv.Itoa(int(sig))
}
