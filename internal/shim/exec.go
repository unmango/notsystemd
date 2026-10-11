package shim

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
)

// Plan is a command a shim runs in its own place: who as, with what
// environment, and whether to wait for it.
type Plan struct {
	Argv []string
	// User and Group name who to run as; empty means the current one.
	User, Group string
	// Env is the environment to run with; nil means the current one.
	Env []string
	// Background starts the command and returns without waiting.
	Background bool
}

// ParseSudo turns sudo's arguments into a Plan. Only the flags software runs
// sudo non-interactively with are understood: -u, -g, -E, -H, -n and
// --preserve-env. The environment is kept, as with -E, apart from HOME, USER
// and LOGNAME, which follow the target user.
func ParseSudo(args []string) (Plan, error) {
	var p Plan
args:
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs an argument", a)
			}
			i++
			return args[i], nil
		}
		var err error
		switch {
		case a == "--":
			p.Argv = args[i+1:]
			return p, checkArgv(p)
		case a == "-u" || a == "--user":
			p.User, err = next()
		case strings.HasPrefix(a, "--user="):
			p.User = strings.TrimPrefix(a, "--user=")
		case a == "-g" || a == "--group":
			p.Group, err = next()
		case strings.HasPrefix(a, "--group="):
			p.Group = strings.TrimPrefix(a, "--group=")
		case strings.HasPrefix(a, "--preserve-env"):
		case strings.HasPrefix(a, "--"):
			switch a {
			case "--non-interactive", "--set-home":
			default:
				return p, fmt.Errorf("unsupported option %s", a)
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			// Bundled short flags, such as -En. A bundle can end in u or g,
			// whose value is the next argument.
			for j, c := range a[1:] {
				switch c {
				case 'E', 'H', 'n', 'S':
				case 'u', 'g':
					v := a[2+j:]
					if v == "" {
						if v, err = next(); err != nil {
							return p, err
						}
					}
					if c == 'u' {
						p.User = v
					} else {
						p.Group = v
					}
					continue args
				default:
					return p, fmt.Errorf("unsupported option -%c", c)
				}
			}
		default:
			p.Argv = args[i:]
			return p, checkArgv(p)
		}
		if err != nil {
			return p, err
		}
	}
	return p, checkArgv(p)
}

func checkArgv(p Plan) error {
	if len(p.Argv) == 0 {
		return fmt.Errorf("no command given")
	}
	return nil
}

// ParseSystemdRun turns systemd-run's arguments into a Plan: the command runs
// in place, as the --uid and --gid given, with --setenv added. Without --wait
// or --pipe it runs in the background, as systemd-run returns once the unit
// has started.
func ParseSystemdRun(args []string) (Plan, error) {
	var p Plan
	wait := false
	var setenv []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			p.Argv = args[i:]
			break
		}
		if a == "--" {
			p.Argv = args[i+1:]
			break
		}
		name, v, hasValue := strings.Cut(a, "=")
		takes := map[string]bool{
			"--unit": true, "-u": true, "--property": true, "-p": true,
			"--uid": true, "--gid": true, "--setenv": true, "-E": true,
			"--description": true, "--slice": true, "--working-directory": true,
			"--nice": true, "--service-type": true, "--on-calendar": true,
		}
		if takes[name] && !hasValue {
			if i+1 >= len(args) {
				return p, fmt.Errorf("%s needs an argument", name)
			}
			i++
			v = args[i]
		}
		switch name {
		case "--wait", "--pipe", "-P", "-t", "--pty":
			wait = true
		case "--uid":
			p.User = v
		case "--gid":
			p.Group = v
		case "--setenv", "-E":
			if !strings.Contains(v, "=") {
				v += "=" + os.Getenv(v)
			}
			setenv = append(setenv, v)
		}
	}
	if len(setenv) > 0 {
		p.Env = append(os.Environ(), setenv...)
	}
	p.Background = !wait
	return p, checkArgv(p)
}

// Exec runs p. A foreground plan replaces the current process, so its exit
// status is the shim's; Exec returns only on failure.
func Exec(e Env, p Plan) int {
	path, err := exec.LookPath(p.Argv[0])
	if err != nil {
		fmt.Fprintln(e.Stderr, err)
		return 127
	}
	env := p.Env
	if env == nil {
		env = os.Environ()
	}
	cred, env, err := credential(p, env)
	if err != nil {
		fmt.Fprintln(e.Stderr, err)
		return 1
	}

	if p.Background {
		cmd := exec.Command(path, p.Argv[1:]...)
		cmd.Args[0] = p.Argv[0]
		cmd.Env = env
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred, Setsid: true}
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(e.Stderr, err)
			return 1
		}
		return 0
	}

	if cred != nil {
		if err := syscall.Setgroups(toInts(cred.Groups)); err != nil {
			fmt.Fprintln(e.Stderr, err)
			return 1
		}
		if err := syscall.Setgid(int(cred.Gid)); err != nil {
			fmt.Fprintln(e.Stderr, err)
			return 1
		}
		if err := syscall.Setuid(int(cred.Uid)); err != nil {
			fmt.Fprintln(e.Stderr, err)
			return 1
		}
	}
	err = syscall.Exec(path, p.Argv, env)
	fmt.Fprintln(e.Stderr, err)
	return 126
}

func toInts(ids []uint32) []int {
	out := make([]int, len(ids))
	for i, id := range ids {
		out[i] = int(id)
	}
	return out
}

// credential resolves who p runs as, and points HOME, USER and LOGNAME at
// them. It returns nil when p runs as the current user.
func credential(p Plan, env []string) (*syscall.Credential, []string, error) {
	if p.User == "" && p.Group == "" {
		return nil, env, nil
	}
	cred := &syscall.Credential{Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid())}
	if p.User != "" {
		u, err := lookupUser(p.User)
		if err != nil {
			return nil, nil, err
		}
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ := strconv.Atoi(u.Gid)
		cred.Uid, cred.Gid = uint32(uid), uint32(gid)
		if groups, err := u.GroupIds(); err == nil {
			for _, g := range groups {
				if id, err := strconv.Atoi(g); err == nil {
					cred.Groups = append(cred.Groups, uint32(id))
				}
			}
		}
		env = setEnv(env, "HOME", u.HomeDir)
		env = setEnv(env, "USER", u.Username)
		env = setEnv(env, "LOGNAME", u.Username)
	}
	if p.Group != "" {
		gid, err := lookupGroup(p.Group)
		if err != nil {
			return nil, nil, err
		}
		cred.Gid = gid
	}
	if os.Getuid() != 0 {
		if cred.Uid != uint32(os.Getuid()) || cred.Gid != uint32(os.Getgid()) {
			return nil, nil, fmt.Errorf("a password is required to run as %s", p.User+p.Group)
		}
		// Already who the plan asks for, and unable to call setgroups.
		return nil, env, nil
	}
	return cred, env, nil
}

// lookupUser takes a name or, as sudo does, "#uid".
func lookupUser(name string) (*user.User, error) {
	if id, ok := strings.CutPrefix(name, "#"); ok {
		return user.LookupId(id)
	}
	return user.Lookup(name)
}

func lookupGroup(name string) (uint32, error) {
	id, isID := strings.CutPrefix(name, "#")
	if !isID {
		g, err := user.LookupGroup(name)
		if err != nil {
			return 0, err
		}
		id = g.Gid
	}
	n, err := strconv.Atoi(id)
	return uint32(n), err
}

func setEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return append(out, key+"="+value)
}
