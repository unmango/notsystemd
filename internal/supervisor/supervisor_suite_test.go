package supervisor_test

import (
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/unmango/notsystemd/internal/notify"
)

// TestMain doubles as the services the specs run: with NOTSYSTEMD_HELPER
// set, the test binary behaves as a Type=notify daemon instead.
func TestMain(m *testing.M) {
	if mode := os.Getenv("NOTSYSTEMD_HELPER"); mode != "" {
		os.Exit(helper(mode))
	}
	os.Exit(m.Run())
}

func helper(mode string) int {
	sock := os.Getenv("NOTIFY_SOCKET")
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM)

	switch mode {
	case "ready":
		notify.Send(sock, "STATUS=warming up")
		notify.Send(sock, "READY=1\nSTATUS=serving")
	case "never-ready":
	case "ready-no-pings":
		notify.Send(sock, "READY=1")
	case "ready-with-pings":
		notify.Send(sock, "READY=1")
		go func() {
			for range time.Tick(200 * time.Millisecond) {
				notify.Send(sock, "WATCHDOG=1")
			}
		}()
	}
	<-sigs
	return 0
}

func TestSupervisor(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Supervisor Suite")
}
