// Package procreg records the process groups a worker starts (mkosi trees,
// build and test VMs) so that a worker restarted after a crash can kill what
// its predecessor left behind: no orphan builds or VMs survive a worker.
package procreg

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Dir is the registry directory (one file per running process group).
type Dir string

// Register records a process group; the returned func removes the record.
func (d Dir) Register(name string, pgid int) func() {
	if d == "" || pgid <= 0 {
		return func() {}
	}
	os.MkdirAll(string(d), 0o700)
	p := filepath.Join(string(d), fmt.Sprintf("%s.%d", sanitize(name), pgid))
	// Record the start time so a recycled PID is never killed by mistake.
	os.WriteFile(p, []byte(strconv.Itoa(pgid)+" "+startTime(pgid)+"\n"), 0o600)
	return func() { os.Remove(p) }
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
}

// startTime is field 22 of /proc/<pid>/stat (clock ticks since boot).
func startTime(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return ""
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 20 {
		return ""
	}
	return f[19]
}

// Reap kills every recorded process group whose leader is still the same
// process, waits briefly for it to exit, and clears the registry. It returns
// how many groups were killed.
func (d Dir) Reap(log *slog.Logger) int {
	ents, err := os.ReadDir(string(d))
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range ents {
		p := filepath.Join(string(d), e.Name())
		b, _ := os.ReadFile(p)
		f := strings.Fields(string(b))
		os.Remove(p)
		if len(f) == 0 {
			continue
		}
		pgid, err := strconv.Atoi(f[0])
		if err != nil || pgid <= 1 {
			continue
		}
		if len(f) > 1 && startTime(pgid) != "" && startTime(pgid) != f[1] {
			continue // PID reused by an unrelated process
		}
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err == nil {
			n++
			if log != nil {
				log.Warn("reaped orphan process group from a previous worker run", "name", e.Name(), "pgid", pgid)
			}
			for i := 0; i < 50 && syscall.Kill(-pgid, 0) == nil; i++ {
				time.Sleep(100 * time.Millisecond)
			}
		} else if !errors.Is(err, syscall.ESRCH) && log != nil {
			log.Error("reap", "pgid", pgid, "err", err)
		}
	}
	return n
}
