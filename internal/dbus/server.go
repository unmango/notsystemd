package dbus

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/unmango/notsystemd/internal/state"
)

// DefaultSocket is where clients look for the system bus.
const DefaultSocket = "/run/dbus/system_bus_socket"

const (
	busName     = "org.freedesktop.DBus"
	busPath     = "/org/freedesktop/DBus"
	systemdName = "org.freedesktop.systemd1"
	// systemdUnique is systemd1's unique name. Clients drop a signal whose
	// sender is not the unique name owning the well-known one they watch.
	systemdUnique = ":1.0"
	managerPath   = "/org/freedesktop/systemd1"
	unitPrefix    = managerPath + "/unit/"

	ifaceIntrospectable = "org.freedesktop.DBus.Introspectable"
	ifacePeer           = "org.freedesktop.DBus.Peer"
	ifaceProperties     = "org.freedesktop.DBus.Properties"
	ifaceManager        = "org.freedesktop.systemd1.Manager"
	ifaceUnit           = "org.freedesktop.systemd1.Unit"
	ifaceService        = "org.freedesktop.systemd1.Service"
)

// Server is a system bus with org.freedesktop.systemd1 on it, serving units'
// ActiveState and StatusText from the state notsystemd records, and signalling
// their changes with PropertiesChanged.
//
// It is the bus as well as the service, so a pod needs no dbus-daemon: the
// two only ever meet here, and nothing else on the bus is reachable.
type Server struct {
	// StateDir is where the units' state files live.
	StateDir string
	// Poll is how often the state directory is read for changes; zero means
	// half a second.
	Poll time.Duration
	Log  *slog.Logger

	mu      sync.Mutex
	clients map[*client]struct{}
	next    int
	guid    string
	// last is each watched unit's state as last signalled.
	last map[string]state.Unit
}

type client struct {
	conn net.Conn
	name string
	mu   sync.Mutex
	// serial numbers the messages sent to this client.
	serial uint32
}

func (c *client) send(m *Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.serial++
	m.Serial = c.serial
	m.Destination = c.name
	b, err := m.Bytes()
	if err != nil {
		return err
	}
	// A client that stops reading must not stall signals to the others.
	c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err = c.conn.Write(b); err != nil {
		// Part of the message may be on the wire, so nothing sent after it
		// would parse; the client's read loop ends when the socket closes.
		c.conn.Close()
	}
	return err
}

// writeTimeout bounds how long a client may take to accept one message.
const writeTimeout = 5 * time.Second

// maxTracked bounds the units a server watches, since clients name them.
const maxTracked = 1024

// validUnitName reports whether a client-supplied name can only refer to a
// file in the state directory.
func validUnitName(name string) bool {
	return name != "" && !strings.ContainsAny(name, "/\x00")
}

// ListenAndServe serves the bus on a Unix socket at path until ctx is done.
func (s *Server) ListenAndServe(ctx context.Context, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	// Any user may connect, as to a system bus.
	if err := os.Chmod(path, 0o666); err != nil {
		l.Close()
		return err
	}
	return s.Serve(ctx, l)
}

// Serve accepts clients on l until ctx is done.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	s.init()
	// Record what is already there before any client can connect, so a
	// change made after that is signalled rather than taken as the baseline.
	s.trackAll()
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	go s.watch(ctx)
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handle(conn)
	}
}

func (s *Server) init() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clients != nil {
		return
	}
	s.clients = map[*client]struct{}{}
	s.last = map[string]state.Unit{}
	// :1.0 is systemd1's.
	s.next = 1
	b := make([]byte, 16)
	rand.Read(b)
	s.guid = hex.EncodeToString(b)
	if s.Log == nil {
		s.Log = slog.New(slog.DiscardHandler)
	}
	if s.Poll == 0 {
		s.Poll = 500 * time.Millisecond
	}
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	if err := s.auth(r, conn); err != nil {
		s.Log.Debug("dbus auth failed", "err", err)
		return
	}
	c := &client{conn: conn}
	defer func() {
		s.mu.Lock()
		delete(s.clients, c)
		s.mu.Unlock()
	}()
	for {
		m, err := ReadMessage(r)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.Log.Debug("dbus read failed", "client", c.name, "err", err)
			}
			return
		}
		if c.name == "" && !(m.Destination == busName && m.Member == "Hello") {
			s.Log.Debug("dbus message before Hello", "member", m.Member)
			return
		}
		m.Sender = c.name
		if err := s.dispatch(c, m); err != nil {
			s.Log.Debug("dbus write failed", "client", c.name, "err", err)
			return
		}
	}
}

// auth runs the SASL exchange that opens a connection. EXTERNAL and ANONYMOUS
// are both accepted without checking who is asking: the socket only exists
// inside the pod, and what it serves is read-only.
func (s *Server) auth(r *bufio.Reader, w io.Writer) error {
	if b, err := r.ReadByte(); err != nil {
		return err
	} else if b != 0 {
		return errors.New("missing credentials byte")
	}
	reply := func(line string) error {
		_, err := io.WriteString(w, line+"\r\n")
		return err
	}
	authed := false
	pending := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return err
		}
		fields := strings.Fields(strings.TrimRight(line, "\r\n"))
		if len(fields) == 0 {
			if err := reply("ERROR"); err != nil {
				return err
			}
			continue
		}
		switch fields[0] {
		case "AUTH":
			switch {
			case len(fields) >= 2 && fields[1] == "EXTERNAL" && len(fields) == 2:
				pending = true
				err = reply("DATA")
			case len(fields) >= 2 && (fields[1] == "EXTERNAL" || fields[1] == "ANONYMOUS"):
				authed = true
				err = reply("OK " + s.guid)
			default:
				err = reply("REJECTED EXTERNAL ANONYMOUS")
			}
		case "DATA":
			if pending {
				pending, authed = false, true
				err = reply("OK " + s.guid)
			} else {
				err = reply("ERROR")
			}
		case "CANCEL":
			pending = false
			err = reply("REJECTED EXTERNAL ANONYMOUS")
		case "NEGOTIATE_UNIX_FD":
			err = reply("ERROR file descriptors are not supported")
		case "BEGIN":
			if authed {
				return nil
			}
			err = reply("ERROR")
		default:
			err = reply("ERROR")
		}
		if err != nil {
			return err
		}
	}
}

func (s *Server) dispatch(c *client, m *Message) error {
	if m.Type != MethodCall {
		return nil
	}
	var reply *Message
	switch m.Destination {
	case busName:
		reply = s.bus(c, m)
		reply.Sender = busName
	case systemdName, systemdUnique:
		reply = s.systemd(m)
		reply.Sender = systemdUnique
	default:
		reply = errorReply(m, "org.freedesktop.DBus.Error.ServiceUnknown",
			fmt.Sprintf("The name %s was not provided by any .service files", m.Destination))
		reply.Sender = busName
	}
	if m.Flags&FlagNoReplyExpected != 0 {
		return nil
	}
	return c.send(reply)
}

func returnOf(m *Message, sig string, body ...any) *Message {
	return &Message{Type: MethodReturn, ReplySerial: m.Serial, Signature: sig, Body: body}
}

func errorReply(m *Message, name, text string) *Message {
	return &Message{Type: Error, ReplySerial: m.Serial, ErrorName: name, Signature: "s", Body: []any{text}}
}

func unknownMethod(m *Message) *Message {
	return errorReply(m, "org.freedesktop.DBus.Error.UnknownMethod",
		fmt.Sprintf("Unknown method %s.%s on %s", m.Interface, m.Member, m.Path))
}

func stringArg(m *Message, i int) (string, bool) {
	if i >= len(m.Body) {
		return "", false
	}
	s, ok := m.Body[i].(string)
	return s, ok
}

// bus answers the methods of org.freedesktop.DBus itself.
func (s *Server) bus(c *client, m *Message) *Message {
	switch m.Interface + "." + m.Member {
	case busName + ".Hello", ".Hello":
		if c.name != "" {
			return errorReply(m, "org.freedesktop.DBus.Error.Failed", "Already handled an Hello message")
		}
		s.mu.Lock()
		c.name = fmt.Sprintf(":1.%d", s.next)
		s.next++
		s.clients[c] = struct{}{}
		s.mu.Unlock()
		return returnOf(m, "s", c.name)
	case busName + ".AddMatch", busName + ".RemoveMatch", ".AddMatch", ".RemoveMatch":
		// Every signal goes to every client, which filters for itself.
		return returnOf(m, "")
	case busName + ".GetNameOwner", ".GetNameOwner":
		name, _ := stringArg(m, 0)
		if owner := s.owner(name); owner != "" {
			return returnOf(m, "s", owner)
		}
		return errorReply(m, "org.freedesktop.DBus.Error.NameHasNoOwner",
			fmt.Sprintf("Could not get owner of name '%s': no such name", name))
	case busName + ".NameHasOwner", ".NameHasOwner":
		name, _ := stringArg(m, 0)
		return returnOf(m, "b", s.owner(name) != "")
	case busName + ".ListNames", ".ListNames":
		names := []any{busName, systemdName, systemdUnique}
		s.mu.Lock()
		for cl := range s.clients {
			names = append(names, cl.name)
		}
		s.mu.Unlock()
		return returnOf(m, "as", names)
	case busName + ".GetId", ".GetId":
		return returnOf(m, "s", s.guid)
	case ifacePeer + ".Ping":
		return returnOf(m, "")
	case ifaceIntrospectable + ".Introspect":
		return returnOf(m, "s", busXML)
	}
	return unknownMethod(m)
}

// owner returns the unique name owning name, or "" when nothing does.
func (s *Server) owner(name string) string {
	switch name {
	case busName:
		return busName
	case systemdName, systemdUnique:
		return systemdUnique
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.clients {
		if c.name == name {
			return name
		}
	}
	return ""
}

// systemd answers org.freedesktop.systemd1.
func (s *Server) systemd(m *Message) *Message {
	path := string(m.Path)
	switch m.Interface + "." + m.Member {
	case ifacePeer + ".Ping", ".Ping":
		return returnOf(m, "")
	case ifaceIntrospectable + ".Introspect", ".Introspect":
		return s.introspect(m, path)
	}

	if path == managerPath {
		switch m.Interface + "." + m.Member {
		case ifaceManager + ".Subscribe", ifaceManager + ".Unsubscribe",
			".Subscribe", ".Unsubscribe":
			// PropertiesChanged goes to every client whether or not it
			// subscribed.
			return returnOf(m, "")
		case ifaceManager + ".LoadUnit", ".LoadUnit":
			name, _ := stringArg(m, 0)
			if !validUnitName(name) {
				return invalidUnit(m, name)
			}
			s.track(name)
			return returnOf(m, "o", UnitPath(name))
		case ifaceManager + ".GetUnit", ".GetUnit":
			name, _ := stringArg(m, 0)
			if !validUnitName(name) {
				return invalidUnit(m, name)
			}
			if _, err := state.Read(s.StateDir, name); err != nil {
				return errorReply(m, "org.freedesktop.systemd1.NoSuchUnit",
					fmt.Sprintf("Unit %s not loaded.", name))
			}
			s.track(name)
			return returnOf(m, "o", UnitPath(name))
		case ifaceProperties + ".Get":
			iface, _ := stringArg(m, 0)
			prop, _ := stringArg(m, 1)
			if v, ok := managerProperties()[prop]; ok && (iface == "" || iface == ifaceManager) {
				return returnOf(m, "v", v)
			}
			return unknownProperty(m, prop)
		case ifaceProperties + ".GetAll":
			return returnOf(m, "a{sv}", dict(managerProperties()))
		}
		return unknownMethod(m)
	}

	name, ok := unitName(path)
	if !ok || !validUnitName(name) {
		return errorReply(m, "org.freedesktop.DBus.Error.UnknownObject",
			fmt.Sprintf("Unknown object '%s'.", path))
	}
	props := unitProperties(s.read(name))
	switch m.Interface + "." + m.Member {
	case ifaceProperties + ".Get":
		iface, _ := stringArg(m, 0)
		prop, _ := stringArg(m, 1)
		for _, i := range []string{ifaceUnit, ifaceService} {
			if iface != "" && iface != i {
				continue
			}
			if v, ok := props[i][prop]; ok {
				return returnOf(m, "v", v)
			}
		}
		return unknownProperty(m, prop)
	case ifaceProperties + ".GetAll":
		iface, _ := stringArg(m, 0)
		all := map[string]Variant{}
		for i, p := range props {
			if iface == "" || iface == i {
				for k, v := range p {
					all[k] = v
				}
			}
		}
		return returnOf(m, "a{sv}", dict(all))
	case ifaceProperties + ".Set":
		return errorReply(m, "org.freedesktop.DBus.Error.PropertyReadOnly", "Properties are read-only.")
	}
	return unknownMethod(m)
}

func unknownProperty(m *Message, prop string) *Message {
	return errorReply(m, "org.freedesktop.DBus.Error.UnknownProperty",
		fmt.Sprintf("Unknown property %s", prop))
}

func managerProperties() map[string]Variant {
	return map[string]Variant{
		"Version":     {"s", "notsystemd"},
		"SystemState": {"s", "running"},
	}
}

// read returns the unit's state, or an inactive unit when the pod does not run
// it, which is how systemd reports a unit it could not find.
func (s *Server) read(name string) state.Unit {
	u, err := state.Read(s.StateDir, name)
	if err != nil {
		return state.Unit{Name: name, ActiveState: "inactive", SubState: "dead"}
	}
	return u
}

func unitProperties(u state.Unit) map[string]map[string]Variant {
	load := "loaded"
	if u.Updated.IsZero() {
		load = "not-found"
	}
	result := u.Result
	if result == "" {
		result = "success"
	}
	return map[string]map[string]Variant{
		ifaceUnit: {
			"Id":          {"s", u.Name},
			"Names":       {"as", []any{u.Name}},
			"Description": {"s", u.Description},
			"LoadState":   {"s", load},
			"ActiveState": {"s", u.ActiveState},
			"SubState":    {"s", u.SubState},
		},
		ifaceService: {
			"StatusText":     {"s", u.StatusText},
			"MainPID":        {"u", uint32(u.MainPID)},
			"Result":         {"s", result},
			"ExecMainStatus": {"i", int32(u.ExitStatus)},
		},
	}
}

func dict(props map[string]Variant) []any {
	entries := make([]any, 0, len(props))
	for k, v := range props {
		entries = append(entries, DictEntry{Key: k, Value: v})
	}
	return entries
}

// track adds a unit to those whose changes are signalled, recording its
// current state so the first signal is a real change.
func (s *Server) track(name string) {
	u := s.read(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.last[name]; !ok && len(s.last) < maxTracked {
		s.last[name] = u
	}
}

// trackAll tracks every unit with a state file.
func (s *Server) trackAll() {
	files, err := filepath.Glob(filepath.Join(s.StateDir, "*.json"))
	if err != nil {
		return
	}
	for _, f := range files {
		s.track(strings.TrimSuffix(filepath.Base(f), ".json"))
	}
}

// watch signals PropertiesChanged for every change to a unit that is in the
// state directory or that a client has asked about.
func (s *Server) watch(ctx context.Context) {
	t := time.NewTicker(s.Poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.trackAll()
		s.mu.Lock()
		names := make([]string, 0, len(s.last))
		for name := range s.last {
			names = append(names, name)
		}
		s.mu.Unlock()
		for _, name := range names {
			s.changed(name)
		}
	}
}

func (s *Server) changed(name string) {
	u := s.read(name)
	s.mu.Lock()
	prev := s.last[name]
	s.last[name] = u
	clients := make([]*client, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()

	// One signal per interface: a client that finds ActiveState unchanged in
	// a signal may skip the rest of it.
	var signals []*Message
	if u.ActiveState != prev.ActiveState || u.SubState != prev.SubState {
		signals = append(signals, propertiesChanged(name, ifaceUnit, map[string]Variant{
			"ActiveState": {"s", u.ActiveState},
			"SubState":    {"s", u.SubState},
		}))
	}
	if u.StatusText != prev.StatusText {
		signals = append(signals, propertiesChanged(name, ifaceService, map[string]Variant{
			"StatusText": {"s", u.StatusText},
		}))
	}
	for _, sig := range signals {
		for _, c := range clients {
			if err := c.send(sig.clone()); err != nil {
				s.Log.Debug("dbus signal failed", "client", c.name, "err", err)
			}
		}
	}
}

func invalidUnit(m *Message, name string) *Message {
	return errorReply(m, "org.freedesktop.DBus.Error.InvalidArgs",
		fmt.Sprintf("Unit name %q is not valid.", name))
}

func propertiesChanged(name, iface string, props map[string]Variant) *Message {
	return &Message{
		Type:      Signal,
		Path:      UnitPath(name),
		Interface: ifaceProperties,
		Member:    "PropertiesChanged",
		Sender:    systemdUnique,
		Signature: "sa{sv}as",
		Body:      []any{iface, dict(props), []any{}},
	}
}

func (m *Message) clone() *Message {
	c := *m
	return &c
}

// UnitPath is the object path systemd gives the unit named name: its name
// with every byte that is not a letter or digit escaped as _xx.
func UnitPath(name string) ObjectPath {
	var b strings.Builder
	b.WriteString(unitPrefix)
	if name == "" {
		b.WriteByte('_')
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' && i > 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "_%02x", c)
		}
	}
	return ObjectPath(b.String())
}

func unitName(path string) (string, bool) {
	label, ok := strings.CutPrefix(path, unitPrefix)
	if !ok || label == "" || strings.Contains(label, "/") {
		return "", false
	}
	if label == "_" {
		return "", true
	}
	var b strings.Builder
	for i := 0; i < len(label); i++ {
		if label[i] != '_' {
			b.WriteByte(label[i])
			continue
		}
		if i+2 >= len(label) {
			return "", false
		}
		x, err := hex.DecodeString(label[i+1 : i+3])
		if err != nil {
			return "", false
		}
		b.Write(x)
		i += 2
	}
	return b.String(), true
}
