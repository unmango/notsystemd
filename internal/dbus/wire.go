// Package dbus is just enough of the D-Bus wire protocol to stand in for a
// system bus with one service on it, org.freedesktop.systemd1, answering from
// the unit state notsystemd records.
//
// Only little-endian messages are read and written, which is what every
// client on the architectures notsystemd runs on sends.
package dbus

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Variant is a D-Bus variant: a value together with its signature.
type Variant struct {
	Sig   string
	Value any
}

// ObjectPath is a value marshalled as an object path (o) rather than a
// string.
type ObjectPath string

// Signature is a value marshalled as a signature (g).
type Signature string

// DictEntry is one {kv} pair of a dictionary.
type DictEntry struct {
	Key, Value any
}

type encoder struct {
	buf []byte
}

func (e *encoder) align(n int) {
	for len(e.buf)%n != 0 {
		e.buf = append(e.buf, 0)
	}
}

func (e *encoder) uint32(v uint32) {
	e.align(4)
	e.buf = binary.LittleEndian.AppendUint32(e.buf, v)
}

func (e *encoder) string(s string) {
	e.uint32(uint32(len(s)))
	e.buf = append(e.buf, s...)
	e.buf = append(e.buf, 0)
}

func (e *encoder) signature(s string) {
	e.buf = append(e.buf, byte(len(s)))
	e.buf = append(e.buf, s...)
	e.buf = append(e.buf, 0)
}

// encode appends v marshalled as the single complete type at the start of
// sig and returns what is left of sig.
func (e *encoder) encode(sig string, v any) (string, error) {
	if sig == "" {
		return "", errors.New("dbus: value without a signature")
	}
	switch sig[0] {
	case 'y':
		b, ok := v.(byte)
		if !ok {
			return "", typeError(sig, v)
		}
		e.buf = append(e.buf, b)
	case 'b':
		b, ok := v.(bool)
		if !ok {
			return "", typeError(sig, v)
		}
		var u uint32
		if b {
			u = 1
		}
		e.uint32(u)
	case 'n', 'q':
		var u uint16
		switch x := v.(type) {
		case int16:
			u = uint16(x)
		case uint16:
			u = x
		default:
			return "", typeError(sig, v)
		}
		e.align(2)
		e.buf = binary.LittleEndian.AppendUint16(e.buf, u)
	case 'i', 'u':
		var u uint32
		switch x := v.(type) {
		case int32:
			u = uint32(x)
		case uint32:
			u = x
		default:
			return "", typeError(sig, v)
		}
		e.uint32(u)
	case 'x', 't', 'd':
		var u uint64
		switch x := v.(type) {
		case int64:
			u = uint64(x)
		case uint64:
			u = x
		case float64:
			u = math.Float64bits(x)
		default:
			return "", typeError(sig, v)
		}
		e.align(8)
		e.buf = binary.LittleEndian.AppendUint64(e.buf, u)
	case 's':
		s, ok := v.(string)
		if !ok {
			return "", typeError(sig, v)
		}
		e.string(s)
	case 'o':
		s, ok := v.(ObjectPath)
		if !ok {
			return "", typeError(sig, v)
		}
		e.string(string(s))
	case 'g':
		s, ok := v.(Signature)
		if !ok {
			return "", typeError(sig, v)
		}
		e.signature(string(s))
	case 'v':
		x, ok := v.(Variant)
		if !ok {
			return "", typeError(sig, v)
		}
		e.signature(x.Sig)
		rest, err := e.encode(x.Sig, x.Value)
		if err != nil {
			return "", err
		}
		if rest != "" {
			return "", fmt.Errorf("dbus: variant signature %q holds more than one type", x.Sig)
		}
	case 'a':
		elem, rest, err := splitType(sig[1:])
		if err != nil {
			return "", err
		}
		items, ok := v.([]any)
		if !ok {
			return "", typeError(sig, v)
		}
		e.uint32(0)
		lenAt := len(e.buf) - 4
		e.align(alignment(elem[0]))
		start := len(e.buf)
		for _, item := range items {
			if _, err := e.encode(elem, item); err != nil {
				return "", err
			}
		}
		binary.LittleEndian.PutUint32(e.buf[lenAt:], uint32(len(e.buf)-start))
		return rest, nil
	case '(':
		typ, rest, err := splitType(sig)
		if err != nil {
			return "", err
		}
		fields, ok := v.([]any)
		if !ok {
			return "", typeError(sig, v)
		}
		e.align(8)
		inner := typ[1 : len(typ)-1]
		for _, f := range fields {
			if inner, err = e.encode(inner, f); err != nil {
				return "", err
			}
		}
		if inner != "" {
			return "", fmt.Errorf("dbus: too few fields for %q", typ)
		}
		return rest, nil
	case '{':
		typ, rest, err := splitType(sig)
		if err != nil {
			return "", err
		}
		entry, ok := v.(DictEntry)
		if !ok {
			return "", typeError(sig, v)
		}
		e.align(8)
		inner, err := e.encode(typ[1:len(typ)-1], entry.Key)
		if err != nil {
			return "", err
		}
		if _, err := e.encode(inner, entry.Value); err != nil {
			return "", err
		}
		return rest, nil
	default:
		return "", fmt.Errorf("dbus: unsupported type %q", sig[0])
	}
	return sig[1:], nil
}

func typeError(sig string, v any) error {
	return fmt.Errorf("dbus: cannot marshal %T as %q", v, sig[0])
}

// Marshal appends values, marshalled as sig, to a body starting at offset 0.
func Marshal(sig string, values ...any) ([]byte, error) {
	e := &encoder{}
	for _, v := range values {
		var err error
		if sig, err = e.encode(sig, v); err != nil {
			return nil, err
		}
	}
	if sig != "" {
		return nil, fmt.Errorf("dbus: no value for %q", sig)
	}
	return e.buf, nil
}

func alignment(c byte) int {
	switch c {
	case 'n', 'q':
		return 2
	case 'b', 'i', 'u', 's', 'o', 'a':
		return 4
	case 'x', 't', 'd', '(', '{':
		return 8
	default:
		return 1
	}
}

// splitType returns the first complete type of sig and the rest.
func splitType(sig string) (string, string, error) {
	if sig == "" {
		return "", "", errors.New("dbus: missing type in signature")
	}
	switch sig[0] {
	case 'a':
		elem, rest, err := splitType(sig[1:])
		return "a" + elem, rest, err
	case '(', '{':
		closer := byte(')')
		if sig[0] == '{' {
			closer = '}'
		}
		depth := 0
		for i := 0; i < len(sig); i++ {
			switch sig[i] {
			case '(', '{':
				depth++
			case ')', '}':
				depth--
				if depth == 0 {
					if sig[i] != closer {
						return "", "", fmt.Errorf("dbus: mismatched %q", sig)
					}
					// An empty struct would decode without reading a byte,
					// so an array of them would never end.
					if i == 1 {
						return "", "", fmt.Errorf("dbus: empty container %q", sig[:2])
					}
					return sig[:i+1], sig[i+1:], nil
				}
			}
		}
		return "", "", fmt.Errorf("dbus: unterminated %q", sig)
	default:
		return sig[:1], sig[1:], nil
	}
}

// maxDepth is how deeply containers may nest, the specification's 32 arrays
// plus 32 structs. A variant names its own signature, so without a limit a
// body could recurse until the stack overflows, which recover cannot catch.
const maxDepth = 64

type decoder struct {
	buf   []byte
	pos   int
	depth int
}

var errShort = errors.New("dbus: message truncated")

func (d *decoder) align(n int) error {
	for d.pos%n != 0 {
		if d.pos >= len(d.buf) {
			return errShort
		}
		d.pos++
	}
	return nil
}

func (d *decoder) take(n int) ([]byte, error) {
	if n < 0 || d.pos+n > len(d.buf) {
		return nil, errShort
	}
	b := d.buf[d.pos : d.pos+n]
	d.pos += n
	return b, nil
}

func (d *decoder) uint32() (uint32, error) {
	if err := d.align(4); err != nil {
		return 0, err
	}
	b, err := d.take(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

func (d *decoder) string() (string, error) {
	n, err := d.uint32()
	if err != nil {
		return "", err
	}
	b, err := d.take(int(n) + 1)
	if err != nil {
		return "", err
	}
	return string(b[:n]), nil
}

func (d *decoder) signature() (string, error) {
	b, err := d.take(1)
	if err != nil {
		return "", err
	}
	s, err := d.take(int(b[0]) + 1)
	if err != nil {
		return "", err
	}
	return string(s[:b[0]]), nil
}

// decode reads one value of the complete type at the start of sig and returns
// it with the rest of sig. Values come back as the types encode takes.
func (d *decoder) decode(sig string) (any, string, error) {
	typ, rest, err := splitType(sig)
	if err != nil {
		return nil, "", err
	}
	if err := d.align(alignment(typ[0])); err != nil {
		return nil, "", err
	}
	switch typ[0] {
	case 'v', 'a', '(', '{':
		if d.depth >= maxDepth {
			return nil, "", errors.New("dbus: values nested too deeply")
		}
		d.depth++
		defer func() { d.depth-- }()
	}
	switch typ[0] {
	case 'y':
		b, err := d.take(1)
		if err != nil {
			return nil, "", err
		}
		return b[0], rest, nil
	case 'b':
		u, err := d.uint32()
		return u != 0, rest, err
	case 'n', 'q':
		b, err := d.take(2)
		if err != nil {
			return nil, "", err
		}
		u := binary.LittleEndian.Uint16(b)
		if typ[0] == 'n' {
			return int16(u), rest, nil
		}
		return u, rest, nil
	case 'i':
		u, err := d.uint32()
		return int32(u), rest, err
	case 'u':
		u, err := d.uint32()
		return u, rest, err
	case 'x', 't', 'd':
		b, err := d.take(8)
		if err != nil {
			return nil, "", err
		}
		u := binary.LittleEndian.Uint64(b)
		switch typ[0] {
		case 'x':
			return int64(u), rest, nil
		case 'd':
			return math.Float64frombits(u), rest, nil
		}
		return u, rest, nil
	case 's':
		s, err := d.string()
		return s, rest, err
	case 'o':
		s, err := d.string()
		return ObjectPath(s), rest, err
	case 'g':
		s, err := d.signature()
		return Signature(s), rest, err
	case 'v':
		s, err := d.signature()
		if err != nil {
			return nil, "", err
		}
		v, left, err := d.decode(s)
		if err != nil {
			return nil, "", err
		}
		if left != "" {
			return nil, "", fmt.Errorf("dbus: variant signature %q holds more than one type", s)
		}
		return Variant{Sig: s, Value: v}, rest, nil
	case 'a':
		n, err := d.uint32()
		if err != nil {
			return nil, "", err
		}
		if err := d.align(alignment(typ[1])); err != nil {
			return nil, "", err
		}
		end := d.pos + int(n)
		if end > len(d.buf) {
			return nil, "", errShort
		}
		items := []any{}
		for d.pos < end {
			v, _, err := d.decode(typ[1:])
			if err != nil {
				return nil, "", err
			}
			items = append(items, v)
		}
		return items, rest, nil
	case '(':
		inner := typ[1 : len(typ)-1]
		fields := []any{}
		for inner != "" {
			var v any
			if v, inner, err = d.decode(inner); err != nil {
				return nil, "", err
			}
			fields = append(fields, v)
		}
		return fields, rest, nil
	case '{':
		k, inner, err := d.decode(typ[1 : len(typ)-1])
		if err != nil {
			return nil, "", err
		}
		v, _, err := d.decode(inner)
		if err != nil {
			return nil, "", err
		}
		return DictEntry{Key: k, Value: v}, rest, nil
	}
	return nil, "", fmt.Errorf("dbus: unsupported type %q", typ[0])
}

// Unmarshal reads a body marshalled as sig.
func Unmarshal(sig string, body []byte) ([]any, error) {
	d := &decoder{buf: body}
	var values []any
	for sig != "" {
		var v any
		var err error
		if v, sig, err = d.decode(sig); err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, nil
}
