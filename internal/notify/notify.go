// Package notify receives sd_notify(3) messages, the datagrams a service
// sends to $NOTIFY_SOCKET to report readiness, status and watchdog pings.
package notify

import (
	"errors"
	"net"
	"os"
	"strings"
)

// Message is one notification: newline-separated KEY=VALUE assignments.
type Message map[string]string

// Parse reads a notification datagram.
func Parse(data []byte) Message {
	m := Message{}
	for _, line := range strings.Split(string(data), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok && key != "" {
			m[key] = value
		}
	}
	return m
}

// Listener owns a notification socket.
type Listener struct {
	Path string
	conn *net.UnixConn
}

// Listen creates the socket at path, replacing any stale one. The socket is
// writable by everyone, because a service commonly drops privileges before
// it notifies and systemd's own socket accepts it either way.
func Listen(path string) (*Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o777); err != nil {
		conn.Close()
		return nil, err
	}
	return &Listener{Path: path, conn: conn}, nil
}

// Messages delivers notifications until the listener is closed.
func (l *Listener) Messages() <-chan Message {
	ch := make(chan Message)
	go func() {
		defer close(ch)
		buf := make([]byte, 64*1024)
		for {
			n, _, err := l.conn.ReadFromUnix(buf)
			if err != nil {
				return
			}
			ch <- Parse(buf[:n])
		}
	}()
	return ch
}

// Close closes the socket and removes it.
func (l *Listener) Close() error {
	err := l.conn.Close()
	os.Remove(l.Path)
	return err
}

// Send writes a notification to the socket at path, as sd_notify does. A
// leading '@' names an abstract socket.
func Send(path, message string) error {
	if strings.HasPrefix(path, "@") {
		path = "\x00" + path[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(message))
	return err
}
