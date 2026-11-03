package procreg

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestReapKillsOrphanGroup(t *testing.T) {
	d := Dir(t.TempDir())
	// A process group with a child, like mkosi and its helpers.
	cmd := exec.Command("/bin/sh", "-c", "sleep 300 & sleep 300; wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pg := cmd.Process.Pid
	d.Register("mkosi-bld_1", pg)
	time.Sleep(200 * time.Millisecond)
	// "worker restarts": a new Dir value on the same path reaps.
	if n := Dir(string(d)).Reap(nil); n != 1 {
		t.Fatalf("reaped %d", n)
	}
	cmd.Wait()
	if syscall.Kill(-pg, 0) == nil {
		t.Fatal("process group still alive")
	}
	if n := d.Reap(nil); n != 0 {
		t.Fatal("registry not cleared")
	}
}

func TestReapSkipsRecycledPID(t *testing.T) {
	d := Dir(t.TempDir())
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Start()
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	done := d.Register("x", cmd.Process.Pid)
	done()
	if d.Reap(nil) != 0 {
		t.Fatal("unregistered group reaped")
	}
}
