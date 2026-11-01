package dbus_test

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/unmango/notsystemd/internal/dbus"
	"github.com/unmango/notsystemd/internal/state"
)

// testClient speaks to the bus the way a client library does.
type testClient struct {
	conn   net.Conn
	r      *bufio.Reader
	serial uint32
	// signals holds the signals read while waiting for a reply.
	signals []*dbus.Message
}

func dial(path string) *testClient {
	conn, err := net.Dial("unix", path)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(conn.Close)
	c := &testClient{conn: conn, r: bufio.NewReader(conn)}
	_, err = io.WriteString(conn, "\x00AUTH EXTERNAL 30\r\n")
	Expect(err).NotTo(HaveOccurred())
	line, err := c.r.ReadString('\n')
	Expect(err).NotTo(HaveOccurred())
	Expect(line).To(HavePrefix("OK "))
	_, err = io.WriteString(conn, "BEGIN\r\n")
	Expect(err).NotTo(HaveOccurred())
	return c
}

func (c *testClient) call(dest string, path dbus.ObjectPath, iface, member, sig string, args ...any) *dbus.Message {
	c.serial++
	m := &dbus.Message{
		Type: dbus.MethodCall, Serial: c.serial, Destination: dest, Path: path,
		Interface: iface, Member: member, Signature: sig, Body: args,
	}
	b, err := m.Bytes()
	Expect(err).NotTo(HaveOccurred())
	_, err = c.conn.Write(b)
	Expect(err).NotTo(HaveOccurred())
	for {
		reply := c.read()
		if reply.Type == dbus.Signal {
			c.signals = append(c.signals, reply)
			continue
		}
		Expect(reply.ReplySerial).To(Equal(c.serial))
		return reply
	}
}

func (c *testClient) read() *dbus.Message {
	Expect(c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))).To(Succeed())
	m, err := dbus.ReadMessage(c.r)
	Expect(err).NotTo(HaveOccurred())
	return m
}

func (c *testClient) signal() *dbus.Message {
	if len(c.signals) > 0 {
		m := c.signals[0]
		c.signals = c.signals[1:]
		return m
	}
	m := c.read()
	Expect(m.Type).To(Equal(dbus.Signal))
	return m
}

const unitPath = dbus.ObjectPath("/org/freedesktop/systemd1/unit/ulp_2dgo_2eservice")

var _ = Describe("Server", func() {
	var (
		dir    string
		client *testClient
	)

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
		Expect(state.Write(dir, state.Unit{
			Name: "ulp-go.service", ActiveState: "activating", SubState: "start",
		})).To(Succeed())

		socket := filepath.Join(GinkgoT().TempDir(), "bus")
		l, err := net.Listen("unix", socket)
		Expect(err).NotTo(HaveOccurred())
		ctx, cancel := context.WithCancel(context.Background())
		DeferCleanup(cancel)
		s := &dbus.Server{StateDir: dir, Poll: 20 * time.Millisecond}
		go s.Serve(ctx, l)

		client = dial(socket)
		reply := client.call("org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", "Hello", "")
		Expect(reply.Type).To(Equal(dbus.MethodReturn))
		Expect(reply.Body).To(Equal([]any{":1.1"}))
	})

	It("names systemd1's owner as the sender of its replies", func() {
		owner := client.call("org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus",
			"GetNameOwner", "s", "org.freedesktop.systemd1")
		Expect(owner.Body).To(Equal([]any{":1.0"}))

		reply := client.call("org.freedesktop.systemd1", "/org/freedesktop/systemd1",
			"org.freedesktop.systemd1.Manager", "LoadUnit", "s", "ulp-go.service")
		Expect(reply.Sender).To(Equal(":1.0"))
		Expect(reply.Destination).To(Equal(":1.1"))
		Expect(reply.Body).To(Equal([]any{unitPath}))
	})

	It("reads a unit's properties from its state file", func() {
		reply := client.call("org.freedesktop.systemd1", unitPath, "org.freedesktop.DBus.Properties",
			"Get", "ss", "org.freedesktop.systemd1.Unit", "ActiveState")
		Expect(reply.Body).To(Equal([]any{dbus.Variant{Sig: "s", Value: "activating"}}))
	})

	It("reports a unit the pod does not run as inactive", func() {
		reply := client.call("org.freedesktop.systemd1", dbus.UnitPath("nginx.service"),
			"org.freedesktop.DBus.Properties", "Get", "ss", "org.freedesktop.systemd1.Unit", "ActiveState")
		Expect(reply.Body).To(Equal([]any{dbus.Variant{Sig: "s", Value: "inactive"}}))
	})

	It("lists both unit interfaces when introspected", func() {
		reply := client.call("org.freedesktop.systemd1", unitPath, "org.freedesktop.DBus.Introspectable",
			"Introspect", "")
		xml := reply.Body[0].(string)
		Expect(xml).To(ContainSubstring(`<interface name="org.freedesktop.systemd1.Unit">`))
		Expect(xml).To(ContainSubstring(`<interface name="org.freedesktop.systemd1.Service">`))
		Expect(xml).To(ContainSubstring(`<signal name="PropertiesChanged">`))
	})

	It("signals ActiveState and StatusText changes separately", func() {
		client.call("org.freedesktop.systemd1", "/org/freedesktop/systemd1",
			"org.freedesktop.systemd1.Manager", "Subscribe", "")
		Expect(state.Write(dir, state.Unit{
			Name: "ulp-go.service", ActiveState: "active", SubState: "running", StatusText: `{"ok":true}`,
		})).To(Succeed())

		unit := client.signal()
		Expect(unit.Sender).To(Equal(":1.0"))
		Expect(unit.Path).To(Equal(unitPath))
		Expect(unit.Member).To(Equal("PropertiesChanged"))
		Expect(unit.Signature).To(Equal("sa{sv}as"))
		Expect(unit.Body[0]).To(Equal("org.freedesktop.systemd1.Unit"))
		Expect(unit.Body[1]).To(ContainElement(dbus.DictEntry{Key: "ActiveState", Value: dbus.Variant{Sig: "s", Value: "active"}}))

		service := client.signal()
		Expect(service.Body[0]).To(Equal("org.freedesktop.systemd1.Service"))
		Expect(service.Body[1]).To(Equal([]any{dbus.DictEntry{Key: "StatusText", Value: dbus.Variant{Sig: "s", Value: `{"ok":true}`}}}))
	})

	It("refuses a unit name that leaves the state directory", func() {
		reply := client.call("org.freedesktop.systemd1", "/org/freedesktop/systemd1",
			"org.freedesktop.systemd1.Manager", "LoadUnit", "s", "../ulp-go.service")
		Expect(reply.ErrorName).To(Equal("org.freedesktop.DBus.Error.InvalidArgs"))

		reply = client.call("org.freedesktop.systemd1", dbus.UnitPath("../ulp-go.service"),
			"org.freedesktop.DBus.Properties", "Get", "ss", "org.freedesktop.systemd1.Unit", "ActiveState")
		Expect(reply.ErrorName).To(Equal("org.freedesktop.DBus.Error.UnknownObject"))
	})

	It("answers a call to another service with an error", func() {
		reply := client.call("org.freedesktop.timedate1", "/org/freedesktop/timedate1",
			"org.freedesktop.DBus.Properties", "Get", "ss", "org.freedesktop.timedate1", "NTP")
		Expect(reply.Type).To(Equal(dbus.Error))
		Expect(reply.ErrorName).To(Equal("org.freedesktop.DBus.Error.ServiceUnknown"))
	})
})

var _ = Describe("ReadMessage", func() {
	It("reads back what Bytes writes", func() {
		m := &dbus.Message{
			Type: dbus.Signal, Serial: 3, Path: unitPath, Interface: "org.freedesktop.DBus.Properties",
			Member: "PropertiesChanged", Sender: ":1.0", Signature: "sa{sv}as",
			Body: []any{"i", []any{}, []any{"x"}},
		}
		b, err := m.Bytes()
		Expect(err).NotTo(HaveOccurred())

		got, err := dbus.ReadMessage(bufio.NewReader(strings.NewReader(string(b))))
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(m))
	})

	It("refuses a message larger than a method call needs", func() {
		header := []byte{'l', 1, 0, 1, 0, 0, 0, 2, 1, 0, 0, 0, 0, 0, 0, 0}
		_, err := dbus.ReadMessage(bufio.NewReader(bytes.NewReader(header)))
		Expect(err).To(MatchError(ContainSubstring("too long")))
	})
})
