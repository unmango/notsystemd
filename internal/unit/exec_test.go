package unit_test

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/unmango/notsystemd/internal/unit"
)

var _ = Describe("ParseExec", func() {
	It("reads prefixes", func() {
		cmds, err := unit.ParseExec("-+/usr/bin/true")
		Expect(err).NotTo(HaveOccurred())
		Expect(cmds).To(HaveLen(1))
		Expect(cmds[0].IgnoreFailure).To(BeTrue())
		Expect(cmds[0].Privileged).To(BeTrue())
		path, argv := cmds[0].Argv(nil)
		Expect(path).To(Equal("/usr/bin/true"))
		Expect(argv).To(Equal([]string{"/usr/bin/true"}))
	})

	It("takes argv[0] from the second word with '@'", func() {
		cmds, err := unit.ParseExec("@/usr/bin/daemon my-daemon --flag")
		Expect(err).NotTo(HaveOccurred())
		path, argv := cmds[0].Argv(nil)
		Expect(path).To(Equal("/usr/bin/daemon"))
		Expect(argv).To(Equal([]string{"my-daemon", "--flag"}))
	})

	It("honours quotes and escapes", func() {
		cmds, err := unit.ParseExec(`/bin/sh -c "echo \"hi there\"" 'single quoted'`)
		Expect(err).NotTo(HaveOccurred())
		_, argv := cmds[0].Argv(nil)
		Expect(argv).To(Equal([]string{"/bin/sh", "-c", `echo "hi there"`, "single quoted"}))
	})

	It("splits commands on a lone semicolon", func() {
		cmds, err := unit.ParseExec(`/bin/a 1 ; /bin/b ";" 2`)
		Expect(err).NotTo(HaveOccurred())
		Expect(cmds).To(HaveLen(2))
		_, argv := cmds[1].Argv(nil)
		Expect(argv).To(Equal([]string{"/bin/b", ";", "2"}))
	})

	It("expands variables the way systemd does", func() {
		cmds, err := unit.ParseExec(`/bin/x $OPTS ${ONE}-suffix "$OPTS" pre$ONE $$`)
		Expect(err).NotTo(HaveOccurred())
		_, argv := cmds[0].Argv(map[string]string{"OPTS": "-a -b", "ONE": "1"})
		Expect(argv).To(Equal([]string{"/bin/x", "-a", "-b", "1-suffix", "$OPTS", "pre$ONE", "$"}))
	})

	It("leaves variables alone with ':'", func() {
		cmds, err := unit.ParseExec(`:/bin/x ${ONE}`)
		Expect(err).NotTo(HaveOccurred())
		_, argv := cmds[0].Argv(map[string]string{"ONE": "1"})
		Expect(argv).To(Equal([]string{"/bin/x", "${ONE}"}))
	})

	It("rejects an unterminated quote", func() {
		_, err := unit.ParseExec(`/bin/x "open`)
		Expect(err).To(MatchError(ContainSubstring("unterminated")))
	})
})

var _ = Describe("ParseEnvironment", func() {
	It("splits quoted assignments", func() {
		env, err := unit.ParseEnvironment(`A=1 "B=two words" C=`)
		Expect(err).NotTo(HaveOccurred())
		Expect(env).To(Equal([]string{"A=1", "B=two words", "C="}))
	})
})

var _ = Describe("ReadEnvironmentFile", func() {
	It("reads the Debian /etc/default style", func() {
		path := filepath.Join(GinkgoT().TempDir(), "env")
		Expect(os.WriteFile(path, []byte(`# comment
; also a comment
PLAIN=value
QUOTED="a \"b\" c"
SINGLE='x y'
export EXPORTED=1
CONTINUED=one\
two
`), 0o644)).To(Succeed())

		env, err := unit.ReadEnvironmentFile(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(env).To(Equal([]string{
			"PLAIN=value",
			`QUOTED=a "b" c`,
			"SINGLE=x y",
			"EXPORTED=1",
			"CONTINUED=onetwo",
		}))
	})
})
