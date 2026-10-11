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
