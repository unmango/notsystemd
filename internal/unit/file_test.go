package unit_test

import (
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/unmango/notsystemd/internal/unit"
)

func value(f unit.File, section, key string) string {
	v, _ := f.Value(section, key)
	return v
}

var _ = Describe("Parse", func() {
	It("reads sections, comments and continuations", func() {
		f, err := unit.Parse(strings.NewReader(`
# leading comment
[Unit]
Description=Example

[Service]
; another comment
ExecStart=/bin/echo one \
  two
Environment=A=1
Environment=B=2
`))
		Expect(err).NotTo(HaveOccurred())
		Expect(value(f, "Unit", "Description")).To(Equal("Example"))
		Expect(value(f, "Service", "ExecStart")).To(Equal("/bin/echo one  two"))
		Expect(f.Values("Service", "Environment")).To(Equal([]string{"A=1", "B=2"}))
	})

	It("resets a list at an empty assignment", func() {
		f, err := unit.Parse(strings.NewReader("[Service]\nExecStartPre=/a\nExecStartPre=\nExecStartPre=/b\n"))
		Expect(err).NotTo(HaveOccurred())
		Expect(f.Values("Service", "ExecStartPre")).To(Equal([]string{"/b"}))
	})

	It("ignores trailing whitespace on headers and values", func() {
		// unifi-core.service.d/cap.conf ships with trailing tabs and no
		// final newline.
		f, err := unit.Parse(strings.NewReader("[Service]\t\nUser=root\t\nAmbientCapabilities="))
		Expect(err).NotTo(HaveOccurred())
		Expect(value(f, "Service", "User")).To(Equal("root"))
		Expect(f.Values("Service", "AmbientCapabilities")).To(BeEmpty())
	})

	It("rejects an assignment outside a section", func() {
		_, err := unit.Parse(strings.NewReader("Key=Value\n"))
		Expect(err).To(MatchError(ContainSubstring("outside of a section")))
	})
})

var _ = Describe("Load", func() {
	It("applies drop-ins in file name order across directories", func() {
		dir := GinkgoT().TempDir()
		path := filepath.Join(dir, "lib", "example.service")
		write := func(p, content string) {
			Expect(os.MkdirAll(filepath.Dir(p), 0o755)).To(Succeed())
			Expect(os.WriteFile(p, []byte(content), 0o644)).To(Succeed())
		}
		write(path, "[Service]\nExecStart=/bin/true\nUser=a\n")
		write(path+".d/10-user.conf", "[Service]\nUser=b\n")
		write(filepath.Join(dir, "etc", "example.service.d", "10-user.conf"), "[Service]\nUser=c\n")
		write(filepath.Join(dir, "etc", "example.service.d", "20-env.conf"), "[Service]\nEnvironment=X=1\n")

		f, err := unit.Load(path, filepath.Join(dir, "etc"))
		Expect(err).NotTo(HaveOccurred())
		Expect(value(f, "Service", "User")).To(Equal("c"))
		Expect(f.Values("Service", "Environment")).To(Equal([]string{"X=1"}))
	})
})
