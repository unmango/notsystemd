package shim_test

import (
	"bytes"
	"os"
	"syscall"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/unmango/notsystemd/internal/shim"
	"github.com/unmango/notsystemd/internal/state"
)

type sent struct {
	pid int
	sig os.Signal
}

var _ = Describe("Systemctl", func() {
	var (
		e              shim.Env
		stdout, stderr *bytes.Buffer
		signals        []sent
	)

	BeforeEach(func() {
		stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}
		signals = nil
		e = shim.Env{
			Stdout:   stdout,
			Stderr:   stderr,
			StateDir: GinkgoT().TempDir(),
			Signal: func(pid int, sig os.Signal) error {
				signals = append(signals, sent{pid, sig})
				return nil
			},
		}
		Expect(state.Write(e.StateDir, state.Unit{
			Name:          "ulp-go.service",
			Description:   "UniFi login",
			ActiveState:   "active",
			SubState:      "running",
			StatusText:    `{"state":"ok"}`,
			MainPID:       42,
			SupervisorPID: 7,
			Result:        "success",
		})).To(Succeed())
		Expect(state.Write(e.StateDir, state.Unit{
			Name:          "unifi.service",
			ActiveState:   "failed",
			SubState:      "failed",
			SupervisorPID: 8,
			Result:        "exit-code",
			ExitStatus:    1,
		})).To(Succeed())
	})

	run := func(args ...string) int { return shim.Systemctl(e, args) }

	It("reports the system as running", func() {
		Expect(run("is-system-running", "--wait")).To(Equal(0))
		Expect(stdout.String()).To(Equal("running\n"))
	})

	Describe("is-active", func() {
		It("succeeds for an active unit, with or without the suffix", func() {
			Expect(run("is-active", "ulp-go")).To(Equal(0))
			Expect(run("is-active", "--quiet", "ulp-go.service")).To(Equal(0))
			Expect(stdout.String()).To(Equal("active\n"))
		})

		It("exits 3 for a failed or unknown unit", func() {
			Expect(run("is-active", "unifi", "unifi-protect")).To(Equal(3))
			Expect(stdout.String()).To(Equal("failed\ninactive\n"))
		})
	})

	It("answers is-enabled from whether the pod runs the unit", func() {
		Expect(run("is-enabled", "ulp-go")).To(Equal(0))
		Expect(run("is-enabled", "unifi-protect")).To(Equal(1))
		Expect(stdout.String()).To(Equal("enabled\ndisabled\n"))
	})

	Describe("show", func() {
		It("prints the properties asked for, in that order", func() {
			Expect(run("show", "ulp-go", "-p", "StatusText,ActiveState", "--property=MainPID")).To(Equal(0))
			Expect(stdout.String()).To(Equal("StatusText={\"state\":\"ok\"}\nActiveState=active\nMainPID=42\n"))
		})

		It("prints bare values with --value", func() {
			Expect(run("show", "--value", "--property=ActiveState", "unifi.service")).To(Equal(0))
			Expect(stdout.String()).To(Equal("failed\n"))
		})

		It("shows an unknown unit as not found", func() {
			Expect(run("show", "-p", "LoadState", "--value", "unifi-protect")).To(Equal(0))
			Expect(stdout.String()).To(Equal("not-found\n"))
		})
	})

	Describe("status", func() {
		It("exits 0 for an active unit", func() {
			Expect(run("status", "ulp-go")).To(Equal(0))
			Expect(stdout.String()).To(ContainSubstring("Active: active (running)"))
			Expect(stdout.String()).To(ContainSubstring("Main PID: 42"))
		})

		It("exits 3 for an inactive one and 4 for an unknown one", func() {
			Expect(run("status", "unifi")).To(Equal(3))
			Expect(run("status", "unifi-protect")).To(Equal(4))
		})
	})

	It("restarts a running unit by stopping its notsystemd", func() {
		Expect(run("restart", "ulp-go")).To(Equal(0))
		Expect(signals).To(Equal([]sent{{7, syscall.SIGTERM}}))
	})

	It("reloads a unit by sending its notsystemd SIGHUP", func() {
		Expect(run("reload", "ulp-go")).To(Equal(0))
		Expect(signals).To(Equal([]sent{{7, syscall.SIGHUP}}))
	})

	It("leaves stopped and unknown units alone", func() {
		Expect(run("stop", "unifi", "unifi-protect")).To(Equal(0))
		Expect(signals).To(BeEmpty())
	})

	It("kills the main process with the signal given", func() {
		Expect(run("kill", "-s", "HUP", "ulp-go")).To(Equal(0))
		Expect(run("kill", "--signal=SIGUSR1", "ulp-go")).To(Equal(0))
		Expect(signals).To(Equal([]sent{{42, syscall.SIGHUP}, {42, syscall.SIGUSR1}}))
	})

	It("accepts the verbs the pod decides, without acting on them", func() {
		for _, verb := range []string{"start", "enable", "mask", "unmask", "daemon-reload", "reset-failed"} {
			Expect(run(verb, "--now", "unifi-protect")).To(Equal(0), verb)
		}
		Expect(run("disable", "--now", "unifi-protect")).To(Equal(0))
		Expect(stderr.String()).To(ContainSubstring("ignoring disable --now"))
		Expect(signals).To(BeEmpty())
	})

	It("fails an unsupported verb", func() {
		Expect(run("list-timers")).To(Equal(1))
		Expect(stderr.String()).To(ContainSubstring("not supported"))
	})
})

var _ = Describe("Timedatectl", func() {
	It("reports NTP as on and synchronised", func() {
		var stdout bytes.Buffer
		e := shim.Env{Stdout: &stdout, Stderr: GinkgoWriter}
		code := shim.Timedatectl(e, []string{"show", "--value", "--property=NTPSynchronized", "--property=NTP"})
		Expect(code).To(Equal(0))
		Expect(stdout.String()).To(Equal("yes\nyes\n"))
	})

	It("accepts set-* verbs without changing anything", func() {
		e := shim.Env{Stdout: GinkgoWriter, Stderr: GinkgoWriter}
		Expect(shim.Timedatectl(e, []string{"set-ntp", "true"})).To(Equal(0))
	})
})
