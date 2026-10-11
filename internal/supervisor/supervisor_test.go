package supervisor_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/unmango/notsystemd/internal/state"
	"github.com/unmango/notsystemd/internal/supervisor"
	"github.com/unmango/notsystemd/internal/unit"
)

type harness struct {
	dir    string
	cancel context.CancelFunc
	reload chan struct{}
	code   chan int
}

// start runs the unit text, with %D replaced by a scratch directory, under
// a supervisor in the background.
func start(text string) *harness {
	dir := GinkgoT().TempDir()
	text = strings.ReplaceAll(text, "%D", dir)
	text = strings.ReplaceAll(text, "%SELF", os.Args[0])

	f, err := unit.Parse(strings.NewReader(text))
	Expect(err).NotTo(HaveOccurred())
	svc, err := unit.NewService("test.service", f)
	Expect(err).NotTo(HaveOccurred())

	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{dir: dir, cancel: cancel, reload: make(chan struct{}, 1), code: make(chan int, 1)}
	s := &supervisor.Supervisor{
		Service:  svc,
		StateDir: filepath.Join(dir, "state"),
		Log:      slog.New(slog.NewTextHandler(GinkgoWriter, nil)),
	}
	go func() {
		defer GinkgoRecover()
		h.code <- s.Run(ctx, h.reload)
	}()
	DeferCleanup(cancel)
	return h
}

func (h *harness) state() state.Unit {
	u, _ := state.Read(filepath.Join(h.dir, "state"), "test.service")
	return u
}

func (h *harness) activeState() string { return h.state().ActiveState }

func (h *harness) file(name string) string {
	data, _ := os.ReadFile(filepath.Join(h.dir, name))
	return strings.TrimSpace(string(data))
}

func (h *harness) exitCode() int {
	var code int
	Eventually(h.code, 10*time.Second).Should(Receive(&code))
	return code
}

var _ = Describe("Supervisor", func() {
	It("runs the lifecycle of a simple service and stops it cleanly", func() {
		h := start(`
[Service]
ExecStartPre=/bin/sh -c 'echo pre > %D/pre'
ExecStart=/bin/sh -c 'echo main > %D/main; exec sleep 30'
ExecStartPost=/bin/sh -c 'echo post > %D/post'
ExecStop=/bin/sh -c 'echo $MAINPID > %D/stop'
ExecStopPost=/bin/sh -c 'echo $SERVICE_RESULT > %D/stoppost'
`)
		Eventually(h.activeState).Should(Equal("active"))
		Expect(h.file("pre")).To(Equal("pre"))
		Expect(h.file("post")).To(Equal("post"))
		Eventually(func() string { return h.file("main") }).Should(Equal("main"))
		mainPID := h.state().MainPID
		Expect(mainPID).To(BeNumerically(">", 0))

		h.cancel()
		Expect(h.exitCode()).To(Equal(0))
		Expect(h.file("stop")).To(Equal(fmt.Sprint(mainPID)))
		Expect(h.file("stoppost")).To(Equal("success"))
		Expect(h.activeState()).To(Equal("inactive"))
	})

	It("fails with the main process's exit status", func() {
		h := start(`
[Service]
ExecStart=/bin/sh -c 'exit 3'
ExecStopPost=/bin/sh -c 'echo $SERVICE_RESULT $EXIT_CODE $EXIT_STATUS > %D/stoppost'
`)
		Expect(h.exitCode()).To(Equal(3))
		Expect(h.file("stoppost")).To(Equal("exit-code exited 3"))
		Expect(h.activeState()).To(Equal("failed"))
	})

	It("does not start when ExecStartPre fails, unless prefixed with '-'", func() {
		h := start(`
[Service]
ExecStartPre=-false
ExecStartPre=/bin/sh -c 'exit 7'
ExecStart=/bin/sh -c 'echo started > %D/main'
ExecStopPost=/bin/sh -c 'echo ran > %D/stoppost'
`)
		Expect(h.exitCode()).To(Equal(7))
		Expect(h.file("main")).To(BeEmpty())
		Expect(h.file("stoppost")).To(Equal("ran"))
	})

	It("passes Environment, EnvironmentFile and expansion through", func() {
		dir := GinkgoT().TempDir()
		envFile := filepath.Join(dir, "env")
		Expect(os.WriteFile(envFile, []byte("B=from-file\nC=\"quoted value\"\n"), 0o644)).To(Succeed())
		h := start(fmt.Sprintf(`
[Service]
Type=oneshot
Environment=A=one B=from-unit "OPTS=x y"
EnvironmentFile=%s
EnvironmentFile=-/does/not/exist
ExecStart=/bin/sh -c 'echo "$A $B $C" > %%D/env; echo "$@" > %%D/args' sh $OPTS
`, envFile))
		Expect(h.exitCode()).To(Equal(0))
		Expect(h.file("env")).To(Equal("one from-file quoted value"))
		Expect(h.file("args")).To(Equal("x y"))
	})

	It("runs oneshot commands in order and exits", func() {
		h := start(`
[Service]
Type=oneshot
ExecStart=/bin/sh -c 'echo 1 >> %D/order'
ExecStart=/bin/sh -c 'echo 2 >> %D/order'
`)
		Expect(h.exitCode()).To(Equal(0))
		Expect(h.file("order")).To(Equal("1\n2"))
		Expect(h.activeState()).To(Equal("inactive"))
	})

	It("keeps a oneshot with RemainAfterExit active until stopped", func() {
		h := start(`
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=true
ExecStop=/bin/sh -c 'echo stopped > %D/stop'
`)
		Eventually(h.activeState).Should(Equal("active"))
		Consistently(h.code, 300*time.Millisecond).ShouldNot(Receive())
		h.cancel()
		Expect(h.exitCode()).To(Equal(0))
		Expect(h.file("stop")).To(Equal("stopped"))
	})

	It("waits for READY=1 from a notify service and records STATUS", func() {
		h := start(`
[Service]
Type=notify
Environment=NOTSYSTEMD_HELPER=ready
ExecStart=%SELF
`)
		Eventually(h.activeState).Should(Equal("active"))
		Eventually(func() string { return h.state().StatusText }).Should(Equal("serving"))
		h.cancel()
		Expect(h.exitCode()).To(Equal(0))
	})

	It("fails a notify service that never becomes ready", func() {
		h := start(`
[Service]
Type=notify
TimeoutStartSec=1
TimeoutStopSec=2
Environment=NOTSYSTEMD_HELPER=never-ready
ExecStart=%SELF
`)
		Expect(h.exitCode()).NotTo(Equal(0))
		Expect(h.state().Result).To(Equal("timeout"))
	})

	It("fails a service that stops pinging its watchdog", func() {
		h := start(`
[Service]
Type=notify
WatchdogSec=500ms
WatchdogSignal=SIGTERM
Environment=NOTSYSTEMD_HELPER=ready-no-pings
ExecStart=%SELF
`)
		Expect(h.exitCode()).NotTo(Equal(0))
		Expect(h.state().Result).To(Equal("watchdog"))
	})

	It("keeps a service that pings its watchdog", func() {
		h := start(`
[Service]
Type=notify
WatchdogSec=1
Environment=NOTSYSTEMD_HELPER=ready-with-pings
ExecStart=%SELF
`)
		Eventually(h.activeState).Should(Equal("active"))
		Consistently(h.code, 2*time.Second).ShouldNot(Receive())
		h.cancel()
		Expect(h.exitCode()).To(Equal(0))
	})

	It("runs ExecReload on reload", func() {
		h := start(`
[Service]
ExecStart=sleep 30
ExecReload=/bin/sh -c 'echo $MAINPID >> %D/reloads'
`)
		Eventually(h.activeState).Should(Equal("active"))
		h.reload <- struct{}{}
		Eventually(func() string { return h.file("reloads") }).Should(Equal(fmt.Sprint(h.state().MainPID)))
		Eventually(h.activeState).Should(Equal("active"))
		h.cancel()
		Expect(h.exitCode()).To(Equal(0))
	})

	It("follows a forking daemon through its PID file", func() {
		h := start(`
[Service]
Type=forking
PIDFile=%D/daemon.pid
ExecStart=/bin/sh -c 'sleep 30 & echo $! > %D/daemon.pid'
`)
		Eventually(h.activeState).Should(Equal("active"))
		Expect(h.state().MainPID).To(Equal(atoi(h.file("daemon.pid"))))
		h.cancel()
		Expect(h.exitCode()).To(Equal(0))
	})

	It("escalates to SIGKILL when a service ignores its stop signal", func() {
		h := start(`
[Service]
TimeoutStopSec=500ms
ExecStart=/bin/sh -c 'trap "" TERM; while :; do sleep 0.1; done'
`)
		Eventually(h.activeState).Should(Equal("active"))
		h.cancel()
		Expect(h.exitCode()).To(Equal(128 + 9))
		Expect(h.state().Result).To(Equal("timeout"))
	})
})

func atoi(s string) int {
	var n int
	fmt.Sscan(s, &n)
	return n
}

var _ = io.Discard
