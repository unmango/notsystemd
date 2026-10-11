package unit

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Service is the part of a service unit notsystemd acts on.
type Service struct {
	// Name is the unit's file name, e.g. "unifi-core.service".
	Name string
	// Description is the [Unit] section's Description=.
	Description string

	// Type is simple, exec, notify, forking or oneshot. notify-reload and
	// dbus are treated as notify and simple.
	Type            string
	RemainAfterExit bool
	PIDFile         string

	ExecStartPre  []Command
	ExecStart     []Command
	ExecStartPost []Command
	ExecReload    []Command
	ExecStop      []Command
	ExecStopPost  []Command

	User             string
	Group            string
	WorkingDirectory string
	// WorkingDirectoryOptional is the '-' prefix on WorkingDirectory=.
	WorkingDirectoryOptional bool
	UMask                    *uint32

	// Environment holds Environment= assignments in order.
	Environment      []string
	EnvironmentFiles []EnvironmentFile

	// Directories maps each of RuntimeDirectory=, StateDirectory=,
	// CacheDirectory=, LogsDirectory= and ConfigurationDirectory= to the
	// absolute paths it names.
	Directories    map[string][]string
	DirectoryModes map[string]uint32

	TimeoutStart time.Duration
	TimeoutStop  time.Duration
	Watchdog     time.Duration

	KillMode       string
	KillSignal     syscall.Signal
	WatchdogSignal syscall.Signal

	// SuccessExitStatuses and SuccessSignals are SuccessExitStatus=: exit
	// statuses and signals counted as a clean exit on top of systemd's own.
	SuccessExitStatuses []int
	SuccessSignals      []syscall.Signal

	// NoFile is LimitNOFILE=, or nil to inherit the container's limit.
	NoFile *syscall.Rlimit
}

// EnvironmentFile is one EnvironmentFile= entry.
type EnvironmentFile struct {
	Path string
	// Optional is the '-' prefix: a missing file is not an error.
	Optional bool
}

// Directory roots, for RuntimeDirectory= and friends as systemd resolves them
// for system units.
var directoryRoots = map[string]string{
	"RuntimeDirectory":       "/run",
	"StateDirectory":         "/var/lib",
	"CacheDirectory":         "/var/cache",
	"LogsDirectory":          "/var/log",
	"ConfigurationDirectory": "/etc",
}

// DefaultTimeout is systemd's DefaultTimeoutStartSec and DefaultTimeoutStopSec.
const DefaultTimeout = 90 * time.Second

// LoadService loads a service unit and its drop-ins. See Load.
func LoadService(path string, dropInDirs ...string) (*Service, error) {
	f, err := Load(path, dropInDirs...)
	if err != nil {
		return nil, err
	}
	return NewService(filepath.Base(path), f)
}

// NewService reads the directives of a service unit named name.
func NewService(name string, f File) (*Service, error) {
	spec := specifiers(name)
	get := func(key string) string {
		v, _ := f.Value("Service", key)
		return spec.Replace(v)
	}

	s := &Service{
		Name:           name,
		Type:           "simple",
		KillMode:       "control-group",
		KillSignal:     syscall.SIGTERM,
		WatchdogSignal: syscall.SIGABRT,
		TimeoutStart:   DefaultTimeout,
		TimeoutStop:    DefaultTimeout,
		Directories:    map[string][]string{},
		DirectoryModes: map[string]uint32{},
	}
	s.Description, _ = f.Value("Unit", "Description")

	if v := get("Type"); v != "" {
		switch v {
		case "simple", "exec", "notify", "forking", "oneshot":
			s.Type = v
		case "notify-reload":
			s.Type = "notify"
		case "dbus", "idle":
			s.Type = "simple"
		default:
			return nil, fmt.Errorf("%s: unsupported Type=%s", name, v)
		}
	}

	var err error
	if s.RemainAfterExit, err = parseBool(get("RemainAfterExit")); err != nil {
		return nil, fmt.Errorf("%s: RemainAfterExit: %w", name, err)
	}
	s.PIDFile = get("PIDFile")

	for key, dst := range map[string]*[]Command{
		"ExecStartPre":  &s.ExecStartPre,
		"ExecStart":     &s.ExecStart,
		"ExecStartPost": &s.ExecStartPost,
		"ExecReload":    &s.ExecReload,
		"ExecStop":      &s.ExecStop,
		"ExecStopPost":  &s.ExecStopPost,
	} {
		for _, v := range f.Values("Service", key) {
			commands, err := ParseExec(spec.Replace(v))
			if err != nil {
				return nil, fmt.Errorf("%s: %s: %w", name, key, err)
			}
			*dst = append(*dst, commands...)
		}
	}
	if len(s.ExecStart) == 0 && s.Type != "oneshot" {
		return nil, fmt.Errorf("%s: no ExecStart=", name)
	}
	if len(s.ExecStart) > 1 && s.Type != "oneshot" {
		return nil, fmt.Errorf("%s: more than one ExecStart= is only allowed for Type=oneshot", name)
	}

	// PermissionsStartOnly= is deprecated, but older units still use it to
	// run every command but ExecStart= with full privileges.
	if pso, _ := parseBool(get("PermissionsStartOnly")); pso {
		for _, list := range [][]Command{s.ExecStartPre, s.ExecStartPost, s.ExecReload, s.ExecStop, s.ExecStopPost} {
			for i := range list {
				list[i].Privileged = true
			}
		}
	}

	s.User = get("User")
	s.Group = get("Group")
	if wd := get("WorkingDirectory"); wd != "" {
		s.WorkingDirectoryOptional = strings.HasPrefix(wd, "-")
		s.WorkingDirectory = strings.TrimPrefix(wd, "-")
	}
	if v := get("UMask"); v != "" {
		m, err := strconv.ParseUint(v, 8, 32)
		if err != nil {
			return nil, fmt.Errorf("%s: UMask: %w", name, err)
		}
		u := uint32(m)
		s.UMask = &u
	}

	for _, v := range f.Values("Service", "Environment") {
		assignments, err := ParseEnvironment(spec.Replace(v))
		if err != nil {
			return nil, fmt.Errorf("%s: Environment: %w", name, err)
		}
		s.Environment = append(s.Environment, assignments...)
	}
	for _, v := range f.Values("Service", "EnvironmentFile") {
		v = spec.Replace(v)
		s.EnvironmentFiles = append(s.EnvironmentFiles, EnvironmentFile{
			Path:     strings.TrimPrefix(v, "-"),
			Optional: strings.HasPrefix(v, "-"),
		})
	}

	for key, root := range directoryRoots {
		for _, v := range f.Values("Service", key) {
			for _, d := range strings.Fields(spec.Replace(v)) {
				// "path:symlink" names extra symlinks; notsystemd ignores them.
				d, _, _ = strings.Cut(d, ":")
				s.Directories[key] = append(s.Directories[key], filepath.Join(root, d))
			}
		}
		mode := uint32(0o755)
		if v := get(key + "Mode"); v != "" {
			m, err := strconv.ParseUint(v, 8, 32)
			if err != nil {
				return nil, fmt.Errorf("%s: %sMode: %w", name, key, err)
			}
			mode = uint32(m)
		}
		s.DirectoryModes[key] = mode
	}

	for key, dst := range map[string]*time.Duration{
		"TimeoutStartSec": &s.TimeoutStart,
		"TimeoutStopSec":  &s.TimeoutStop,
		"WatchdogSec":     &s.Watchdog,
	} {
		if v := get(key); v != "" {
			if *dst, err = ParseDuration(v); err != nil {
				return nil, fmt.Errorf("%s: %s: %w", name, key, err)
			}
		}
	}
	if v := get("TimeoutSec"); v != "" {
		d, err := ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("%s: TimeoutSec: %w", name, err)
		}
		if _, ok := f.Value("Service", "TimeoutStartSec"); !ok {
			s.TimeoutStart = d
		}
		if _, ok := f.Value("Service", "TimeoutStopSec"); !ok {
			s.TimeoutStop = d
		}
	}

	if v := get("KillMode"); v != "" {
		switch v {
		case "control-group", "mixed", "process", "none":
			s.KillMode = v
		default:
			return nil, fmt.Errorf("%s: unsupported KillMode=%s", name, v)
		}
	}
	for key, dst := range map[string]*syscall.Signal{
		"KillSignal":     &s.KillSignal,
		"WatchdogSignal": &s.WatchdogSignal,
	} {
		if v := get(key); v != "" {
			if *dst, err = ParseSignal(v); err != nil {
				return nil, fmt.Errorf("%s: %s: %w", name, key, err)
			}
		}
	}

	for _, v := range f.Values("Service", "SuccessExitStatus") {
		for _, word := range strings.Fields(v) {
			if n, err := strconv.Atoi(word); err == nil {
				s.SuccessExitStatuses = append(s.SuccessExitStatuses, n)
				continue
			}
			sig, err := ParseSignal(word)
			if err != nil {
				return nil, fmt.Errorf("%s: SuccessExitStatus: %w", name, err)
			}
			s.SuccessSignals = append(s.SuccessSignals, sig)
		}
	}

	if v := get("LimitNOFILE"); v != "" {
		if s.NoFile, err = ParseLimit(v); err != nil {
			return nil, fmt.Errorf("%s: LimitNOFILE: %w", name, err)
		}
	}

	return s, nil
}

// ParseLimit reads a resource limit as systemd.exec(5) writes it: one value
// for both the soft and hard limit, or "soft:hard", each a number or
// "infinity".
func ParseLimit(v string) (*syscall.Rlimit, error) {
	one := func(v string) (uint64, error) {
		if v == "infinity" {
			return ^uint64(0), nil
		}
		return strconv.ParseUint(v, 10, 64)
	}
	softText, hardText, pair := strings.Cut(v, ":")
	soft, err := one(softText)
	if err != nil {
		return nil, err
	}
	hard := soft
	if pair {
		if hard, err = one(hardText); err != nil {
			return nil, err
		}
	}
	return &syscall.Rlimit{Cur: soft, Max: hard}, nil
}

// specifiers returns a replacer for the unit specifiers that make sense
// outside systemd. See systemd.unit(5).
func specifiers(name string) *strings.Replacer {
	prefix := strings.TrimSuffix(name, filepath.Ext(name))
	instance := ""
	if p, i, ok := strings.Cut(prefix, "@"); ok {
		prefix, instance = p, i
	}
	return strings.NewReplacer(
		"%%", "%",
		"%n", name,
		"%N", strings.TrimSuffix(name, filepath.Ext(name)),
		"%p", prefix,
		"%P", prefix,
		"%i", instance,
		"%I", instance,
		"%t", "/run",
		"%S", "/var/lib",
		"%C", "/var/cache",
		"%L", "/var/log",
		"%E", "/etc",
		"%T", "/tmp",
		"%V", "/var/tmp",
	)
}

func parseBool(v string) (bool, error) {
	switch strings.ToLower(v) {
	case "", "0", "no", "false", "off", "n", "f":
		return false, nil
	case "1", "yes", "true", "on", "y", "t":
		return true, nil
	}
	return false, fmt.Errorf("%q is not a boolean", v)
}

var durationUnits = map[string]time.Duration{
	"us": time.Microsecond, "usec": time.Microsecond,
	"ms": time.Millisecond, "msec": time.Millisecond,
	"s": time.Second, "sec": time.Second, "second": time.Second, "seconds": time.Second,
	"m": time.Minute, "min": time.Minute, "minute": time.Minute, "minutes": time.Minute,
	"h": time.Hour, "hr": time.Hour, "hour": time.Hour, "hours": time.Hour,
	"d": 24 * time.Hour, "day": 24 * time.Hour, "days": 24 * time.Hour,
}

// ParseDuration parses a systemd time span such as "90", "2min" or
// "1min 30s". A bare number is seconds, and "infinity" and "0" mean no
// limit, which is returned as 0.
func ParseDuration(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "infinity" {
		return 0, nil
	}
	if n, err := strconv.ParseFloat(v, 64); err == nil {
		return time.Duration(n * float64(time.Second)), nil
	}

	var total time.Duration
	rest := strings.ReplaceAll(v, " ", "")
	for rest != "" {
		i := 0
		for i < len(rest) && (rest[i] >= '0' && rest[i] <= '9' || rest[i] == '.') {
			i++
		}
		j := i
		for j < len(rest) && rest[j] >= 'a' && rest[j] <= 'z' {
			j++
		}
		n, err := strconv.ParseFloat(rest[:i], 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not a time span", v)
		}
		unit, ok := durationUnits[rest[i:j]]
		if !ok {
			return 0, fmt.Errorf("%q: unknown unit %q", v, rest[i:j])
		}
		total += time.Duration(n * float64(unit))
		rest = rest[j:]
	}
	return total, nil
}

var signals = map[string]syscall.Signal{
	"SIGABRT": syscall.SIGABRT, "SIGHUP": syscall.SIGHUP, "SIGINT": syscall.SIGINT,
	"SIGKILL": syscall.SIGKILL, "SIGQUIT": syscall.SIGQUIT, "SIGTERM": syscall.SIGTERM,
	"SIGUSR1": syscall.SIGUSR1, "SIGUSR2": syscall.SIGUSR2, "SIGWINCH": syscall.SIGWINCH,
	"SIGCONT": syscall.SIGCONT, "SIGSTOP": syscall.SIGSTOP,
}

// ParseSignal parses a signal name with or without its SIG prefix, or a
// signal number.
func ParseSignal(v string) (syscall.Signal, error) {
	if n, err := strconv.Atoi(v); err == nil {
		return syscall.Signal(n), nil
	}
	if !strings.HasPrefix(v, "SIG") {
		v = "SIG" + v
	}
	if s, ok := signals[v]; ok {
		return s, nil
	}
	return 0, fmt.Errorf("unknown signal %q", v)
}
