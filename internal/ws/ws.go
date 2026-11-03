// Package ws is a minimal RFC 6455 WebSocket server used to stream the live
// preview's RFB (VNC) traffic to noVNC in the browser. Binary messages only.
package ws

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

const guid = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Conn is a server-side WebSocket connection.
type Conn struct {
	c   net.Conn
	br  *bufio.Reader
	wmu sync.Mutex
	buf []byte // unread payload of the current message
}

// MaxFrame bounds a single frame from the client (RFB input events are tiny).
const MaxFrame = 1 << 20

// Upgrade performs the handshake. checkOrigin must approve the Origin header
// (browsers send cookies on cross-site WebSocket requests).
func Upgrade(w http.ResponseWriter, r *http.Request, checkOrigin func(string) bool) (*Conn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || !headerHas(r.Header, "Connection", "upgrade") {
		http.Error(w, "websocket upgrade required", http.StatusUpgradeRequired)
		return nil, errors.New("ws: not an upgrade request")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		http.Error(w, "unsupported websocket version", http.StatusBadRequest)
		return nil, errors.New("ws: bad version")
	}
	if checkOrigin != nil && !checkOrigin(r.Header.Get("Origin")) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return nil, errors.New("ws: origin not allowed")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if len(key) == 0 {
		http.Error(w, "missing key", http.StatusBadRequest)
		return nil, errors.New("ws: missing key")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("ws: hijacking unsupported")
	}
	c, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	h := sha1.Sum([]byte(key + guid))
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(h[:]) + "\r\n"
	if headerHas(r.Header, "Sec-WebSocket-Protocol", "binary") {
		resp += "Sec-WebSocket-Protocol: binary\r\n"
	}
	if _, err := c.Write([]byte(resp + "\r\n")); err != nil {
		c.Close()
		return nil, err
	}
	return &Conn{c: c, br: rw.Reader}, nil
}

func headerHas(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// Read returns payload bytes of binary/continuation frames, answering pings
// and returning io.EOF on close.
func (c *Conn) Read(p []byte) (int, error) {
	for len(c.buf) == 0 {
		op, payload, err := c.frame()
		if err != nil {
			return 0, err
		}
		switch op {
		case 0x0, 0x1, 0x2:
			c.buf = payload
		case 0x8:
			c.writeFrame(0x8, nil)
			return 0, io.EOF
		case 0x9:
			c.writeFrame(0xA, payload)
		}
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

func (c *Conn) frame() (byte, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(c.br, h[:]); err != nil {
		return 0, nil, err
	}
	op := h[0] & 0x0f
	if h[1]&0x80 == 0 {
		return 0, nil, errors.New("ws: client frames must be masked")
	}
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return 0, nil, err
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if n > MaxFrame {
		return 0, nil, errors.New("ws: frame too large")
	}
	var mask [4]byte
	if _, err := io.ReadFull(c.br, mask[:]); err != nil {
		return 0, nil, err
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(c.br, p); err != nil {
		return 0, nil, err
	}
	for i := range p {
		p[i] ^= mask[i%4]
	}
	return op, p, nil
}

// Write sends p as one binary message.
func (c *Conn) Write(p []byte) (int, error) {
	if len(p) == 0 { // io.Copy from a bufio.Reader issues empty writes; they are not messages
		return 0, nil
	}
	if err := c.writeFrame(0x2, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *Conn) writeFrame(op byte, p []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	h := []byte{0x80 | op}
	switch n := len(p); {
	case n < 126:
		h = append(h, byte(n))
	case n < 1<<16:
		h = append(h, 126, byte(n>>8), byte(n))
	default:
		h = append(h, 127)
		h = binary.BigEndian.AppendUint64(h, uint64(n))
	}
	if _, err := c.c.Write(h); err != nil {
		return err
	}
	_, err := c.c.Write(p)
	return err
}

// Close sends a close frame and closes the connection.
func (c *Conn) Close() error {
	c.writeFrame(0x8, []byte{0x03, 0xe8})
	return c.c.Close()
}
