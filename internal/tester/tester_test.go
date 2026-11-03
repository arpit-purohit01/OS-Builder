package tester

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const bootLog = "\x00[    1.2] kernel stuff\n" +
	"FOUNDRY-TEST version=1.0.0 id=ubuntu image=ci-server usr=/dev/mapper/usr verity=verified slot=ci-server_1.0.0 root=/dev/mapper/root crypt=yes tpm2=yes sb=0 health=active bootcomplete=active state=running entry=ci-server_1.0.0.efi failed=\n" +
	"FOUNDRY-TEST cmdline=quiet usrhash=abc console=ttyS0\n" +
	"FOUNDRY-REBOOTING\n" +
	"[   99.0] reboot: Restarting system\n" +
	"FOUNDRY-TEST version=1.0.0 id=ubuntu usr=/dev/mapper/usr verity=verified slot=ci-server_1.0.0 root=/dev/mapper/root crypt=yes tpm2=yes sb=0 health=active bootcomplete=active state=running entry=ci-server_1.0.0+2-1.efi failed=\n" +
	"FOUNDRY-TEST cmdline=quiet usrhash=abc\n" +
	"FOUNDRY-TEST rebooted=yes\n" +
	"FOUNDRY-BOOT-OK\n"

func TestWatchAndCheck(t *testing.T) {
	p := filepath.Join(t.TempDir(), "serial.log")
	os.WriteFile(p, []byte(bootLog), 0o644)
	ss := watch(context.Background(), p, Expect{Reports: 2, PerBoot: time.Minute}, make(chan error), nil, "t")
	boots, fail := ss.Boots, ss.Failure
	if fail != "" || len(boots) != 2 {
		t.Fatalf("boots=%d fail=%q", len(boots), fail)
	}
	if boots[0].Fields["crypt"] != "yes" || !strings.Contains(boots[0].Cmdline, "usrhash=") || boots[1].Fields["rebooted"] != "yes" {
		t.Fatalf("parse: %+v", boots)
	}
	sc := Scenario{Reboot: true, Version: "1.0.0", Slot: "ci-server_1.0.0", Crypt: "yes", TPM2: "yes"}
	if f := check(sc, boots); len(f) != 0 {
		t.Fatalf("unexpected failures: %v", f)
	}
	sc.SBEnforced = true
	if f := check(sc, boots); len(f) != 2 {
		t.Fatalf("SB expectation: %v", f)
	}
	bad := strings.Replace(bootLog, "failed=\n", "failed=foo.service\n", 1)
	bad = strings.Replace(bad, "state=running", "state=degraded", 1)
	os.WriteFile(p, []byte(bad), 0o644)
	boots = watch(context.Background(), p, Expect{Reports: 2, PerBoot: time.Minute}, make(chan error), nil, "t").Boots
	if f := check(Scenario{Reboot: true}, boots); len(f) != 2 || !strings.Contains(strings.Join(f, ";"), "foo.service") {
		t.Fatalf("degraded: %v", f)
	}
}

func TestWatchTimeoutAndExit(t *testing.T) {
	p := filepath.Join(t.TempDir(), "serial.log")
	os.WriteFile(p, []byte("nothing\n"), 0o644)
	fail := watch(context.Background(), p, Expect{Reports: 1, PerBoot: 10 * time.Millisecond}, make(chan error), nil, "t").Failure
	if !strings.Contains(fail, "no progress") {
		t.Fatalf("timeout: %q", fail)
	}
	ex := make(chan error, 1)
	ex <- nil
	fail = watch(context.Background(), p, Expect{Reports: 1, PerBoot: time.Minute}, ex, nil, "t").Failure
	if !strings.Contains(fail, "exited") {
		t.Fatalf("exit: %q", fail)
	}
}

func TestWatchMatchForbidAndEvents(t *testing.T) {
	p := filepath.Join(t.TempDir(), "serial.log")
	os.WriteFile(p, []byte("BdsDxe\x00 booting\r\nFOUNDRY-UPDATED ci-server 1.0.1\nFOUNDRY-TRIAL-FAILED version=1.0.1 entry=x+2-1.efi\n[  1.0] device-mapper: verity: 253:0: data block 0 is corrupted\npartial"), 0o644)
	s := watch(context.Background(), p, Expect{Match: verityRe, PerBoot: time.Minute}, make(chan error), nil, "t")
	if s.Matched == "" || len(s.Events) != 2 {
		t.Fatalf("%+v", s)
	}
	s = watch(context.Background(), p, Expect{Match: refusedRe, Forbid: regexp.MustCompile("FOUNDRY-UPDATED"), PerBoot: time.Minute}, make(chan error), nil, "t")
	if !strings.Contains(s.Failure, "unexpected output") {
		t.Fatalf("forbid: %+v", s)
	}
	if !unlockRefusedRe.MatchString(ansiRe.ReplaceAllString("[\x1b[0;1;31mFAILED\x1b[0m] Failed to start \x1b[0;1;39msystemd-cryptsetup…vice\x1b[0m - Cryptography Setup for root.", "")) {
		t.Fatal("ansi stripping")
	}
	if !unlockRefusedRe.MatchString("Please enter passphrase for disk root (root):") || !refusedRe.MatchString("Error loading \\EFI\\Linux\\x.efi: Security Violation") {
		t.Fatal("regexes")
	}
}
