// Package preview runs live previews: a signed image booted in a throwaway
// VM (copy-on-write overlay, own vTPM, no network) whose screen is exported
// over VNC on a unix socket. Sessions are short (default 15 min), limited
// per org and host, and stopped after inactivity.
package preview

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"foundry.dev/foundry/internal/blob"
	"foundry.dev/foundry/internal/cache"
	"foundry.dev/foundry/internal/imgutil"
	"foundry.dev/foundry/internal/tester"
)

var (
	ErrQuota    = errors.New("preview: too many active previews")
	ErrNotFound = errors.New("preview: no such session")
)

// Manager owns the preview VMs of one host.
type Manager struct {
	VM          tester.Config
	Blobs       blob.Store
	Dir         string        // images and session state
	TTL         time.Duration // session length
	Idle        time.Duration // stop after no client traffic
	MaxSessions int
	PerOrg      int

	mu       sync.Mutex
	sessions map[string]*Session
	images   cache.Group
}

// Info is a session's public state.
type Info struct {
	ID      string    `json:"id"`
	OrgID   string    `json:"orgId"`
	BuildID string    `json:"buildId"`
	Started time.Time `json:"startedAt"`
	Expires time.Time `json:"expiresAt"`
	State   string    `json:"state"` // starting | running | stopped
	Reason  string    `json:"reason,omitempty"`
}

// Session is one running preview.
type Session struct {
	Info
	sock     string
	cancel   context.CancelFunc
	lastIO   atomic.Int64
	done     chan struct{}
	machine  *tester.Machine
	attached atomic.Int32
}

func (m *Manager) init() {
	if m.sessions == nil {
		m.sessions = map[string]*Session{}
	}
	if m.TTL == 0 {
		m.TTL = 15 * time.Minute
	}
	if m.Idle == 0 {
		m.Idle = 3 * time.Minute
	}
	if m.MaxSessions == 0 {
		m.MaxSessions = 4
	}
	if m.PerOrg == 0 {
		m.PerOrg = 2
	}
}

// image returns a local raw copy of a signed disk (decompressed once).
func (m *Manager) image(ctx context.Context, digest string) (string, error) {
	hex := strings.TrimPrefix(digest, "sha256:")
	p := filepath.Join(m.Dir, "images", hex+".raw")
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	_, err, _ := m.images.Do(ctx, digest, func() (any, error) {
		if _, err := os.Stat(p); err == nil {
			return nil, nil
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return nil, err
		}
		src, release, err := blob.Local(ctx, m.Blobs, digest, filepath.Dir(p))
		if err != nil {
			return nil, err
		}
		defer release()
		tmp := p + ".tmp"
		if err := imgutil.Unzstd(ctx, src, tmp); err != nil {
			return nil, err
		}
		return nil, os.Rename(tmp, p)
	})
	return p, err
}

func newID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return "pvw_" + hex.EncodeToString(b)
}

// Start boots a preview of a signed disk (digest of disk.raw.zst).
func (m *Manager) Start(ctx context.Context, orgID, buildID, diskDigest string, memMiB int) (Info, error) {
	m.mu.Lock()
	m.init()
	active, org := 0, 0
	for _, s := range m.sessions {
		if s.State != "stopped" {
			active++
			if s.OrgID == orgID {
				org++
			}
		}
	}
	if active >= m.MaxSessions || org >= m.PerOrg {
		m.mu.Unlock()
		return Info{}, ErrQuota
	}
	s := &Session{Info: Info{ID: newID(), OrgID: orgID, BuildID: buildID, Started: time.Now().UTC(), State: "starting"}, done: make(chan struct{})}
	s.Expires = s.Started.Add(m.TTL)
	m.sessions[s.ID] = s
	m.mu.Unlock()

	img, err := m.image(ctx, diskDigest)
	if err != nil {
		m.finish(s, "image: "+err.Error())
		return Info{}, err
	}
	c := m.VM
	c.WorkDir = filepath.Join(m.Dir, "sessions")
	os.MkdirAll(c.WorkDir, 0o700)
	if memMiB > 0 {
		c.MemoryMiB = memMiB
	}
	c.ExtraDiskGiB = 8
	mach, err := tester.NewMachine(ctx, c, s.ID, img, false, true, false)
	if err != nil {
		m.finish(s, err.Error())
		return Info{}, err
	}
	s.machine = mach
	// unix socket paths are limited to 108 bytes
	sd, err := os.MkdirTemp("", "fpv-")
	if err != nil {
		mach.Close()
		m.finish(s, err.Error())
		return Info{}, err
	}
	s.sock = filepath.Join(sd, "vnc")
	vctx, cancel := context.WithDeadline(context.Background(), s.Expires)
	s.cancel = cancel
	display := []string{"-vnc", "unix:" + s.sock, "-device", "VGA", "-device", "qemu-xhci", "-device", "usb-tablet", "-device", "usb-kbd"}
	cmd, stopTPM, err := mach.Command(vctx, filepath.Join(sd, "serial.log"), "", display)
	if err != nil {
		cancel()
		mach.Close()
		os.RemoveAll(sd)
		m.finish(s, err.Error())
		return Info{}, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		stopTPM()
		mach.Close()
		os.RemoveAll(sd)
		m.finish(s, err.Error())
		return Info{}, err
	}
	s.lastIO.Store(time.Now().UnixNano())
	m.mu.Lock()
	s.State = "running"
	m.mu.Unlock()
	go func() {
		err := cmd.Wait()
		stopTPM()
		mach.Close()
		os.RemoveAll(sd)
		reason := "ended"
		if vctx.Err() == context.DeadlineExceeded {
			reason = "time limit reached"
		} else if err != nil && vctx.Err() == nil {
			reason = "VM exited: " + err.Error()
		}
		cancel()
		m.finish(s, reason)
	}()
	go m.watchIdle(s, vctx)
	return m.Get(s.ID)
}

func (m *Manager) watchIdle(s *Session, ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s.attached.Load() == 0 && time.Since(time.Unix(0, s.lastIO.Load())) > m.Idle {
				m.mu.Lock()
				s.Reason = "stopped after inactivity"
				m.mu.Unlock()
				s.cancel()
				return
			}
		}
	}
}

func (m *Manager) finish(s *Session, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.State == "stopped" {
		return
	}
	s.State = "stopped"
	if s.Reason == "" {
		s.Reason = reason
	}
	close(s.done)
}

// Get returns a copy of a session's public state.
func (m *Manager) Get(id string) (Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	s, ok := m.sessions[id]
	if !ok {
		return Info{}, ErrNotFound
	}
	return s.Info, nil
}

// Stop ends a session.
func (m *Manager) Stop(id string) error {
	m.mu.Lock()
	m.init()
	s, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	if s.cancel != nil {
		s.cancel()
	}
	return nil
}

// Attach connects to the session's VNC socket; traffic keeps it alive.
func (m *Manager) Attach(ctx context.Context, id string) (io.ReadWriteCloser, error) {
	m.mu.Lock()
	m.init()
	s, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok || s.State != "running" {
		return nil, ErrNotFound
	}
	var c net.Conn
	var err error
	for i := 0; i < 100; i++ { // QEMU creates the socket shortly after start
		if c, err = net.Dial("unix", s.sock); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.done:
			return nil, fmt.Errorf("preview: session ended: %s", s.Reason)
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err != nil {
		return nil, err
	}
	s.attached.Add(1)
	return &tracked{Conn: c, s: s}, nil
}

type tracked struct {
	net.Conn
	s    *Session
	once sync.Once
}

func (t *tracked) Read(p []byte) (int, error) {
	n, err := t.Conn.Read(p)
	t.s.lastIO.Store(time.Now().UnixNano())
	return n, err
}

func (t *tracked) Write(p []byte) (int, error) {
	t.s.lastIO.Store(time.Now().UnixNano())
	return t.Conn.Write(p)
}

func (t *tracked) Close() error {
	t.once.Do(func() { t.s.attached.Add(-1); t.s.lastIO.Store(time.Now().UnixNano()) })
	return t.Conn.Close()
}

// Reap forgets stopped sessions older than an hour.
func (m *Manager) Reap() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		if s.State == "stopped" && time.Since(s.Expires) > time.Hour {
			delete(m.sessions, id)
		}
	}
}

// Shutdown stops every session.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	var ss []*Session
	for _, s := range m.sessions {
		ss = append(ss, s)
	}
	m.mu.Unlock()
	for _, s := range ss {
		if s.cancel != nil {
			s.cancel()
		}
		<-s.done
	}
}
