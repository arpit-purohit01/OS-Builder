// Package signclient talks to cmd/signer. It never sees key material: it
// sends build IDs and artifact digests and receives digests of signed
// outputs written to the shared blob store.
package signclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Client implements finalize.Signer over HTTP (TCP or a unix socket).
type Client struct {
	BaseURL string // http://127.0.0.1:7443 or unix:///run/foundry/signer.sock
	Token   string
	HTTP    *http.Client
}

// New creates a client; unix:// URLs dial a socket.
func New(url, token string) *Client {
	c := &Client{BaseURL: strings.TrimRight(url, "/"), Token: token, HTTP: &http.Client{Timeout: 10 * time.Minute}}
	if sock, ok := strings.CutPrefix(url, "unix://"); ok {
		c.BaseURL = "http://signer"
		c.HTTP.Transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}}
	}
	return c
}

func (c *Client) call(ctx context.Context, buildID, op string, body map[string]string) (map[string]string, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/v1/builds/"+buildID+"/"+op, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]string
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("signer %s: HTTP %d", op, resp.StatusCode)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("signer %s refused (HTTP %d): %s", op, resp.StatusCode, out["error"])
	}
	return out, nil
}

func (c *Client) SignUKI(ctx context.Context, buildID, digest string) (string, error) {
	r, err := c.call(ctx, buildID, "uki", map[string]string{"digest": digest})
	return r["digest"], err
}

// InstallerUKI signs the installer variant of the build's UKI (live system
// with the installer; fixed command line chosen by the signer).
func (c *Client) InstallerUKI(ctx context.Context, buildID, digest string) (string, error) {
	r, err := c.call(ctx, buildID, "uki", map[string]string{"digest": digest, "variant": "installer"})
	return r["digest"], err
}

func (c *Client) Bootloader(ctx context.Context, buildID string) (string, error) {
	r, err := c.call(ctx, buildID, "bootloader", nil)
	return r["digest"], err
}

func (c *Client) VeritySig(ctx context.Context, buildID, roothash string) (string, error) {
	r, err := c.call(ctx, buildID, "verity", map[string]string{"roothash": roothash})
	return r["digest"], err
}

func (c *Client) Enrollment(ctx context.Context, buildID string) (map[string]string, error) {
	return c.call(ctx, buildID, "enrollment", nil)
}

func (c *Client) SignBlob(ctx context.Context, buildID, digest string) (string, error) {
	r, err := c.call(ctx, buildID, "blob", map[string]string{"digest": digest})
	return r["digest"], err
}

func (c *Client) PublicKeys(ctx context.Context, buildID string) (map[string]string, error) {
	return c.call(ctx, buildID, "public-keys", nil)
}
