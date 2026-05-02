package wyoming

import (
	"bufio"
	"fmt"
	"net"
	"time"
)

// Conn is a buffered Wyoming protocol connection. Methods are NOT safe
// for concurrent calls in the same direction; a typical reader/writer
// pattern uses one goroutine per direction.
type Conn struct {
	conn net.Conn
	br   *bufio.Reader
}

// Dial opens a TCP connection to a Wyoming server at addr (host:port)
// with the given dial timeout. A zero timeout means no timeout.
func Dial(addr string, timeout time.Duration) (*Conn, error) {
	var c net.Conn
	var err error
	if timeout > 0 {
		c, err = net.DialTimeout("tcp", addr, timeout)
	} else {
		c, err = net.Dial("tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("wyoming: dial %s: %w", addr, err)
	}
	return NewConn(c), nil
}

// NewConn wraps an existing net.Conn — useful in tests with net.Pipe.
func NewConn(c net.Conn) *Conn {
	return &Conn{conn: c, br: bufio.NewReader(c)}
}

// Read returns the next event on the connection.
func (c *Conn) Read() (Event, error) { return ReadEvent(c.br) }

// Write sends a single event on the connection.
func (c *Conn) Write(ev Event) error { return WriteEvent(c.conn, ev) }

// SetDeadline sets the read+write deadline on the underlying socket.
func (c *Conn) SetDeadline(t time.Time) error { return c.conn.SetDeadline(t) }

// SetReadDeadline sets the read deadline on the underlying socket.
func (c *Conn) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }

// SetWriteDeadline sets the write deadline on the underlying socket.
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.conn.SetWriteDeadline(t) }

// Close closes the connection.
func (c *Conn) Close() error { return c.conn.Close() }
