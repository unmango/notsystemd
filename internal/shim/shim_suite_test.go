package shim_test

import (
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/unmango/notsystemd/internal/shim"
)

// TestMain doubles as a process that runs a plan, since Exec replaces the
// process that calls it: with NOTSYSTEMD_SHIM_HELPER set, the test binary
// runs that command through Exec.
func TestMain(m *testing.M) {
	if cmd := os.Getenv("NOTSYSTEMD_SHIM_HELPER"); cmd != "" {
		os.Exit(shim.Exec(shim.Default(), shim.Plan{Argv: []string{"sh", "-c", cmd}}))
	}
	os.Exit(m.Run())
}

func TestShim(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Shim Suite")
}
