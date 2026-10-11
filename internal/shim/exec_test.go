package shim_test

import (
	"os"
	"os/exec"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/unmango/notsystemd/internal/shim"
)

var _ = Describe("ParseSudo", func() {
	DescribeTable("finds the user and the command",
		func(args []string, user, group string, argv []string) {
			p, err := shim.ParseSudo(args)
			Expect(err).NotTo(HaveOccurred())
			Expect(p.User).To(Equal(user))
			Expect(p.Group).To(Equal(group))
			Expect(p.Argv).To(Equal(argv))
		},
		Entry("unifi-core's -E -n", []string{"-E", "-n", "systemctl", "restart", "unifi"}, "", "", []string{"systemctl", "restart", "unifi"}),
		Entry("-u", []string{"-u", "postgres", "createuser", "-d", "unifi-core"}, "postgres", "", []string{"createuser", "-d", "unifi-core"}),
		Entry("bundled -nu", []string{"-nu", "postgres", "psql"}, "postgres", "", []string{"psql"}),
		Entry("attached -upostgres", []string{"-upostgres", "psql"}, "postgres", "", []string{"psql"}),
		Entry("long options", []string{"--user=unifi", "--group", "ssl-cert", "--", "-x"}, "unifi", "ssl-cert", []string{"-x"}),
		Entry("arguments after the command are its own", []string{"-n", "sed", "-i", "-e", "s/a/b/", "f"}, "", "", []string{"sed", "-i", "-e", "s/a/b/", "f"}),
	)

	It("refuses options it cannot honour", func() {
		_, err := shim.ParseSudo([]string{"-i", "sh"})
		Expect(err).To(MatchError(ContainSubstring("-i")))
	})

	It("needs a command", func() {
		_, err := shim.ParseSudo([]string{"-u", "postgres"})
		Expect(err).To(MatchError(ContainSubstring("no command")))
	})
})

var _ = Describe("ParseSystemdRun", func() {
	It("waits with --wait or --pipe", func() {
		p, err := shim.ParseSystemdRun([]string{"--wait", "--pipe", "--unit=abc", "-p", "Type=oneshot", "/bin/sh", "-c", "true"})
		Expect(err).NotTo(HaveOccurred())
		Expect(p.Background).To(BeFalse())
		Expect(p.Argv).To(Equal([]string{"/bin/sh", "-c", "true"}))
	})

	It("runs in the background otherwise", func() {
		p, err := shim.ParseSystemdRun([]string{"--unit", "abc", "--uid=unifi", "--setenv=A=1", "--", "job"})
		Expect(err).NotTo(HaveOccurred())
		Expect(p.Background).To(BeTrue())
		Expect(p.User).To(Equal("unifi"))
		Expect(p.Argv).To(Equal([]string{"job"}))
		Expect(p.Env).To(ContainElement("A=1"))
	})
})

var _ = Describe("Exec", func() {
	It("replaces the process, so the command's status is the shim's", func() {
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), "NOTSYSTEMD_SHIM_HELPER=exit 7")
		err := cmd.Run()
		var exitErr *exec.ExitError
		Expect(err).To(BeAssignableToTypeOf(exitErr))
		Expect(cmd.ProcessState.ExitCode()).To(Equal(7))
	})

	It("starts a background plan and returns", func() {
		out := filepath.Join(GinkgoT().TempDir(), "ran")
		code := shim.Exec(shim.Env{Stderr: GinkgoWriter}, shim.Plan{
			Argv:       []string{"sh", "-c", "echo ok > " + out},
			Background: true,
		})
		Expect(code).To(Equal(0))
		Eventually(func() (string, error) {
			b, err := os.ReadFile(out)
			return string(b), err
		}).Should(Equal("ok\n"))
	})

	It("fails a command that is not on PATH", func() {
		code := shim.Exec(shim.Env{Stderr: GinkgoWriter}, shim.Plan{Argv: []string{"no-such-command-here"}})
		Expect(code).To(Equal(127))
	})
})
