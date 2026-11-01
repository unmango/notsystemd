package dbus

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Type is a message type.
type Type byte

const (
	MethodCall   Type = 1
	MethodReturn Type = 2
	Error        Type = 3
	Signal       Type = 4
)

// FlagNoReplyExpected says the sender of a method call does not want a reply.
const FlagNoReplyExpected = 0x1

// Header field codes.
const (
	fieldPath        = 1
	fieldInterface   = 2
	fieldMember      = 3
	fieldErrorName   = 4
	fieldReplySerial = 5
	fieldDestination = 6
	fieldSender      = 7
	fieldSignature   = 8
	fieldUnixFDs     = 9
)

// Message is one D-Bus message. Body holds the arguments, decoded according
// to Signature.
type Message struct {
	Type        Type
	Flags       byte
	Serial      uint32
	Path        ObjectPath
	Interface   string
	Member      string
	ErrorName   string
	ReplySerial uint32
	Destination string
	Sender      string
	Signature   string
	Body        []any
}

// maxMessage is the largest message read. The specification allows 128 MiB,
// but clients of this bus only send small method calls, and the buffer is
// allocated from the header before the body arrives.
const maxMessage = 1 << 20

// ReadMessage reads one message from r.
func ReadMessage(r *bufio.Reader) (*Message, error) {
	fixed := make([]byte, 16)
	if _, err := io.ReadFull(r, fixed); err != nil {
		return nil, err
	}
	if fixed[0] != 'l' {
		return nil, fmt.Errorf("dbus: unsupported byte order %q", fixed[0])
	}
	bodyLen := binary.LittleEndian.Uint32(fixed[4:])
	fieldsLen := binary.LittleEndian.Uint32(fixed[12:])
	headerLen := 16 + int(fieldsLen)
	padded := (headerLen + 7) &^ 7
	if uint64(padded)+uint64(bodyLen) > maxMessage {
		return nil, errors.New("dbus: message too long")
	}
	buf := make([]byte, padded+int(bodyLen))
	copy(buf, fixed)
	if _, err := io.ReadFull(r, buf[16:]); err != nil {
		return nil, err
	}

	m := &Message{
		Type:   Type(fixed[1]),
		Flags:  fixed[2],
		Serial: binary.LittleEndian.Uint32(fixed[8:]),
	}
	d := &decoder{buf: buf[:headerLen], pos: 12}
	fields, _, err := d.decode("a(yv)")
	if err != nil {
		return nil, err
	}
	for _, f := range fields.([]any) {
		field := f.([]any)
		v := field[1].(Variant).Value
		switch field[0].(byte) {
		case fieldPath:
			m.Path, _ = v.(ObjectPath)
		case fieldInterface:
			m.Interface, _ = v.(string)
		case fieldMember:
			m.Member, _ = v.(string)
		case fieldErrorName:
			m.ErrorName, _ = v.(string)
		case fieldReplySerial:
			m.ReplySerial, _ = v.(uint32)
		case fieldDestination:
			m.Destination, _ = v.(string)
		case fieldSender:
			m.Sender, _ = v.(string)
		case fieldSignature:
			sig, _ := v.(Signature)
			m.Signature = string(sig)
		case fieldUnixFDs:
			return nil, errors.New("dbus: file descriptors are not supported")
		}
	}
	if m.Body, err = Unmarshal(m.Signature, buf[padded:]); err != nil {
		return nil, err
	}
	return m, nil
}

// Bytes marshals m.
func (m *Message) Bytes() ([]byte, error) {
	body, err := Marshal(m.Signature, m.Body...)
	if err != nil {
		return nil, err
	}
	var fields []any
	field := func(code byte, sig string, v any) {
		fields = append(fields, []any{code, Variant{Sig: sig, Value: v}})
	}
	if m.Path != "" {
		field(fieldPath, "o", m.Path)
	}
	if m.Interface != "" {
		field(fieldInterface, "s", m.Interface)
	}
	if m.Member != "" {
		field(fieldMember, "s", m.Member)
	}
	if m.ErrorName != "" {
		field(fieldErrorName, "s", m.ErrorName)
	}
	if m.ReplySerial != 0 {
		field(fieldReplySerial, "u", m.ReplySerial)
	}
	if m.Destination != "" {
		field(fieldDestination, "s", m.Destination)
	}
	if m.Sender != "" {
		field(fieldSender, "s", m.Sender)
	}
	if m.Signature != "" {
		field(fieldSignature, "g", Signature(m.Signature))
	}

	e := &encoder{buf: []byte{'l', byte(m.Type), m.Flags, 1}}
	e.uint32(uint32(len(body)))
	e.uint32(m.Serial)
	if _, err := e.encode("a(yv)", fields); err != nil {
		return nil, err
	}
	e.align(8)
	return append(e.buf, body...), nil
}
