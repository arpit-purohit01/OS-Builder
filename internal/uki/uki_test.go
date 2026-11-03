package uki

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectAndPredict(t *testing.T) {
	if _, err := exec.LookPath("ukify"); err != nil {
		t.Skip("ukify missing")
	}
	if _, err := os.Stat("/usr/lib/systemd/systemd-measure"); err != nil {
		t.Skip("systemd-measure missing")
	}
	dir := t.TempDir()
	linux := filepath.Join(dir, "linux")
	os.WriteFile(linux, bytes.Repeat([]byte("K"), 1000), 0o644)
	out := filepath.Join(dir, "u.efi")
	rh := strings.Repeat("ef", 32)
	if b, err := exec.Command("ukify", "build", "--linux="+linux, "--cmdline=ro usrhash="+rh, "--uname=6.8.0", "--os-release=ID=x\n", "--output="+out).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, b)
	}
	info, err := Inspect(out)
	if err != nil || info.Usrhash != rh || info.Uname != "6.8.0" {
		t.Fatalf("%v %+v", err, info)
	}
	p, err := PredictPCR11(context.Background(), "", out)
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 4 || len(p["enter-initrd"]) != 64 || p["enter-initrd"] == p["enter-initrd:leave-initrd"] {
		t.Fatalf("%v", p)
	}
	// Changing the command line changes the prediction.
	out2 := filepath.Join(dir, "u2.efi")
	exec.Command("ukify", "build", "--linux="+linux, "--cmdline=ro usrhash="+rh+" quiet", "--uname=6.8.0", "--os-release=ID=x\n", "--output="+out2).Run()
	p2, _ := PredictPCR11(context.Background(), "", out2)
	if p2["enter-initrd"] == p["enter-initrd"] {
		t.Fatal("prediction ignores the command line")
	}
	if _, err := Sections(linux); err == nil {
		t.Fatal("non-PE accepted")
	}
}
