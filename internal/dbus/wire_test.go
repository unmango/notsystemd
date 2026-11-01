package dbus_test

import (
	"bytes"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/unmango/notsystemd/internal/dbus"
)

var _ = Describe("Marshal", func() {
	It("round-trips a PropertiesChanged body", func() {
		body := []any{
			"org.freedesktop.systemd1.Unit",
			[]any{dbus.DictEntry{Key: "ActiveState", Value: dbus.Variant{Sig: "s", Value: "active"}}},
			[]any{},
		}
		b, err := dbus.Marshal("sa{sv}as", body...)
		Expect(err).NotTo(HaveOccurred())

		Expect(dbus.Unmarshal("sa{sv}as", b)).To(Equal(body))
	})

	It("pads a dictionary entry to eight bytes after the array length", func() {
		b, err := dbus.Marshal("a{sv}", []any{dbus.DictEntry{Key: "k", Value: dbus.Variant{Sig: "u", Value: uint32(7)}}})
		Expect(err).NotTo(HaveOccurred())

		// The length counts from the first entry, after the padding.
		Expect(b[:8]).To(Equal([]byte{16, 0, 0, 0, 0, 0, 0, 0}))
		Expect(b).To(HaveLen(24))
	})

	It("rejects a value of the wrong type", func() {
		_, err := dbus.Marshal("s", uint32(1))
		Expect(err).To(MatchError(ContainSubstring("cannot marshal uint32")))
	})
})

var _ = Describe("Unmarshal", func() {
	It("rejects an array of empty structs", func() {
		_, err := dbus.Unmarshal("a()", []byte{8, 0, 0, 0, 0, 0, 0, 0})
		Expect(err).To(MatchError(ContainSubstring("empty container")))
	})

	It("rejects variants nested past the specification's limit", func() {
		// Each level is a variant whose signature is "v".
		body := bytes.Repeat([]byte{1, 'v', 0}, 100000)
		_, err := dbus.Unmarshal("v", body)
		Expect(err).To(MatchError(ContainSubstring("nested too deeply")))
	})
})

var _ = Describe("UnitPath", func() {
	It("escapes every byte that is not a letter or digit", func() {
		Expect(dbus.UnitPath("ulp-go.service")).To(Equal(dbus.ObjectPath("/org/freedesktop/systemd1/unit/ulp_2dgo_2eservice")))
	})

	It("escapes a leading digit", func() {
		Expect(dbus.UnitPath("1.service")).To(Equal(dbus.ObjectPath("/org/freedesktop/systemd1/unit/_31_2eservice")))
	})
})
