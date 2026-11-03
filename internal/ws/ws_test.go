package ws

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEcho(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r, func(o string) bool { return o == "http://ok.test" })
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c)
	}))
	defer srv.Close()
	dial := func(origin string) (net.Conn, *bufio.Reader, string) {
		c, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		k := make([]byte, 16)
		rand.Read(k)
		io.WriteString(c, "GET / HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Protocol: binary\r\nOrigin: "+origin+"\r\nSec-WebSocket-Key: "+base64.StdEncoding.EncodeToString(k)+"\r\n\r\n")
		br := bufio.NewReader(c)
		status, _ := br.ReadString('\n')
		for {
			l, _ := br.ReadString('\n')
			if l == "\r\n" || l == "" {
				break
			}
		}
		return c, br, status
	}
	if _, _, st := dial("http://evil.test"); !strings.Contains(st, "403") {
		t.Fatalf("cross-origin upgrade: %q", st)
	}
	c, br, st := dial("http://ok.test")
	if !strings.Contains(st, "101") {
		t.Fatal(st)
	}
	msg := []byte("RFB 003.008\n")
	mask := []byte{1, 2, 3, 4}
	frame := []byte{0x82, 0x80 | byte(len(msg))}
	frame = append(frame, mask...)
	for i, b := range msg {
		frame = append(frame, b^mask[i%4])
	}
	c.Write(frame)
	h := make([]byte, 2)
	io.ReadFull(br, h)
	p := make([]byte, h[1])
	io.ReadFull(br, p)
	if h[0] != 0x82 || string(p) != string(msg) {
		t.Fatalf("echo %x %q", h, p)
	}
	// unmasked client frames are a protocol violation
	c.Write([]byte{0x82, 0x01, 'x'})
	io.ReadFull(br, h)
	if h[0] != 0x88 {
		t.Fatalf("expected close, got %x", h)
	}
}
