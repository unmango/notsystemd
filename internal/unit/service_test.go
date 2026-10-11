package unit_test

import (
	"strings"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/unmango/notsystemd/internal/unit"
)

func service(name, text string) (*unit.Service, error) {
	f, err := unit.Parse(strings.NewReader(text))
	if err != nil {
		return nil, err
	}
	return unit.NewService(name, f)
}

var _ = Describe("NewService", func() {
	It("reads a typical notify service", func() {
		s, err := service("unifi-core.service", `
[Unit]
Description=UniFi Core

[Service]
Type=notify
User=unifi-core
WorkingDirectory=-/usr/share/unifi-core
ExecStartPre=+/usr/share/unifi-core/app/hooks/pre-start
ExecStart=/usr/bin/node24 /usr/share/unifi-core/app/service.js
ExecStopPost=/usr/share/unifi-core/app/hooks/post-stop
EnvironmentFile=-/etc/default/unifi-core
RuntimeDirectory=%N
RuntimeDirectoryMode=0750
WatchdogSec=2min
TimeoutSec=300
KillSignal=SIGINT
`)
		Expect(err).NotTo(HaveOccurred())
		Expect(s.Description).To(Equal("UniFi Core"))
		Expect(s.Type).To(Equal("notify"))
		Expect(s.User).To(Equal("unifi-core"))
		Expect(s.WorkingDirectory).To(Equal("/usr/share/unifi-core"))
		Expect(s.WorkingDirectoryOptional).To(BeTrue())
		Expect(s.ExecStartPre[0].Privileged).To(BeTrue())
		Expect(s.EnvironmentFiles).To(Equal([]unit.EnvironmentFile{{Path: "/etc/default/unifi-core", Optional: true}}))
		Expect(s.Directories["RuntimeDirectory"]).To(Equal([]string{"/run/unifi-core"}))
		Expect(s.DirectoryModes["RuntimeDirectory"]).To(Equal(uint32(0o750)))
		Expect(s.Watchdog).To(Equal(2 * time.Minute))
		Expect(s.TimeoutStart).To(Equal(300 * time.Second))
		Expect(s.TimeoutStop).To(Equal(300 * time.Second))
		Expect(s.KillSignal).To(Equal(syscall.SIGINT))
	})

	It("expands instance specifiers", func() {
		s, err := service("postgresql@14-main.service", "[Service]\nExecStart=/usr/bin/pg_ctlcluster %i start\n")
		Expect(err).NotTo(HaveOccurred())
		_, argv := s.ExecStart[0].Argv(nil)
		Expect(argv).To(Equal([]string{"/usr/bin/pg_ctlcluster", "14-main", "start"}))
	})

	It("marks every command but ExecStart privileged under PermissionsStartOnly", func() {
		s, err := service("x.service", "[Service]\nPermissionsStartOnly=true\nExecStartPre=/a\nExecStart=/b\n")
		Expect(err).NotTo(HaveOccurred())
		Expect(s.ExecStartPre[0].Privileged).To(BeTrue())
		Expect(s.ExecStart[0].Privileged).To(BeFalse())
	})

	It("reads UniFi's ULP-style unit layout", func() {
		// Spaces around '=', a leading space in ExecStart=, an empty
		// Wants= and no final newline, as uid-agent.service ships.
		s, err := service("uid-agent.service", "[Unit]\nDescription = UID Agent service\nWants = \n[Service]\n"+
			"ExecStart= /usr/lib/uid-agent/scripts/service/start.sh\nType = notify\nPIDFile = /run/uid-agent.pid\n"+
			"WatchdogSec = 180\nKillMode = process\nLimitNOFILE=65536")
		Expect(err).NotTo(HaveOccurred())
		Expect(s.Description).To(Equal("UID Agent service"))
		Expect(s.Type).To(Equal("notify"))
		Expect(s.PIDFile).To(Equal("/run/uid-agent.pid"))
		Expect(s.Watchdog).To(Equal(180 * time.Second))
		Expect(s.KillMode).To(Equal("process"))
		Expect(s.NoFile).To(Equal(&syscall.Rlimit{Cur: 65536, Max: 65536}))
		_, argv := s.ExecStart[0].Argv(nil)
		Expect(argv).To(Equal([]string{"/usr/lib/uid-agent/scripts/service/start.sh"}))
	})

	It("reads SuccessExitStatus= statuses and signals", func() {
		s, err := service("unifi.service", "[Service]\nExecStart=/a\nSuccessExitStatus=143\nSuccessExitStatus=75 SIGUSR1\n")
		Expect(err).NotTo(HaveOccurred())
		Expect(s.SuccessExitStatuses).To(Equal([]int{143, 75}))
		Expect(s.SuccessSignals).To(Equal([]syscall.Signal{syscall.SIGUSR1}))
	})

	It("gives oneshot units no start timeout unless they set one", func() {
		s, err := service("x.service", "[Service]\nType=oneshot\nExecStart=/a\n")
		Expect(err).NotTo(HaveOccurred())
		Expect(s.TimeoutStart).To(BeZero())
		Expect(s.TimeoutStop).To(Equal(unit.DefaultTimeout))
		s, err = service("x.service", "[Service]\nType=oneshot\nExecStart=/a\nTimeoutStartSec=5\n")
		Expect(err).NotTo(HaveOccurred())
		Expect(s.TimeoutStart).To(Equal(5 * time.Second))
	})

	It("requires ExecStart unless oneshot", func() {
		_, err := service("x.service", "[Service]\nType=simple\n")
		Expect(err).To(MatchError(ContainSubstring("no ExecStart")))
		_, err = service("x.service", "[Service]\nType=oneshot\nExecStartPre=/a\n")
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = DescribeTable("ParseDuration",
	func(in string, want time.Duration) {
		Expect(unit.ParseDuration(in)).To(Equal(want))
	},
	Entry("bare seconds", "90", 90*time.Second),
	Entry("minutes", "2min", 2*time.Minute),
	Entry("compound", "1min 30s", 90*time.Second),
	Entry("milliseconds", "500ms", 500*time.Millisecond),
	Entry("infinity", "infinity", time.Duration(0)),
)

var _ = DescribeTable("ParseLimit",
	func(in string, want syscall.Rlimit) {
		Expect(unit.ParseLimit(in)).To(Equal(&want))
	},
	Entry("one value", "65536", syscall.Rlimit{Cur: 65536, Max: 65536}),
	Entry("soft and hard", "1024:4096", syscall.Rlimit{Cur: 1024, Max: 4096}),
	Entry("infinity", "infinity", syscall.Rlimit{Cur: ^uint64(0), Max: ^uint64(0)}),
)
