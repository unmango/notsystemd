package shim

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

// Timedatectl answers timedatectl's read verbs. A container's clock is the
// node's, which keeps it synchronised, so NTP reads as on and synchronised, and
// the set-* verbs succeed without changing anything.
func Timedatectl(e Env, args []string) int {
	var props []string
	var value bool
	var verb string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--value":
			value = true
		case a == "-p" || a == "--property":
			if i+1 < len(args) {
				i++
				props = append(props, strings.Split(args[i], ",")...)
			}
		case strings.HasPrefix(a, "--property="):
			props = append(props, strings.Split(strings.TrimPrefix(a, "--property="), ",")...)
		case strings.HasPrefix(a, "-"):
		case verb == "":
			verb = a
		}
	}
	if verb == "" {
		verb = "status"
	}

	now := time.Now()
	tz := timezone()
	all := [][2]string{
		{"Timezone", tz},
		{"LocalRTC", "no"},
		{"CanNTP", "yes"},
		{"NTP", "yes"},
		{"NTPSynchronized", "yes"},
		{"TimeUSec", now.Format("Mon 2006-01-02 15:04:05 MST")},
		{"RTCTimeUSec", now.UTC().Format("Mon 2006-01-02 15:04:05")},
	}

	switch verb {
	case "show":
		if len(props) > 0 {
			var picked [][2]string
			for _, want := range props {
				if i := slices.IndexFunc(all, func(p [2]string) bool { return p[0] == want }); i >= 0 {
					picked = append(picked, all[i])
				}
			}
			all = picked
		}
		for _, p := range all {
			if value {
				fmt.Fprintln(e.Stdout, p[1])
			} else {
				fmt.Fprintf(e.Stdout, "%s=%s\n", p[0], p[1])
			}
		}
		return 0
	case "status":
		fmt.Fprintf(e.Stdout, "               Local time: %s\n", now.Format("Mon 2006-01-02 15:04:05 MST"))
		fmt.Fprintf(e.Stdout, "           Universal time: %s\n", now.UTC().Format("Mon 2006-01-02 15:04:05 MST"))
		fmt.Fprintf(e.Stdout, "                Time zone: %s\n", tz)
		fmt.Fprintln(e.Stdout, "System clock synchronized: yes")
		fmt.Fprintln(e.Stdout, "              NTP service: active")
		fmt.Fprintln(e.Stdout, "          RTC in local TZ: no")
		return 0
	case "set-ntp", "set-timezone", "set-local-rtc", "set-time":
		return 0
	case "list-timezones":
		fmt.Fprintln(e.Stdout, tz)
		return 0
	default:
		fmt.Fprintf(e.Stderr, "timedatectl: %s is not supported by notsystemd\n", verb)
		return 1
	}
}

// timezone is $TZ, else the zone /etc/localtime links to, else UTC.
func timezone() string {
	if tz := os.Getenv("TZ"); tz != "" {
		return strings.TrimPrefix(tz, ":")
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if _, zone, ok := strings.Cut(target, "zoneinfo/"); ok {
			return zone
		}
	}
	return "UTC"
}
