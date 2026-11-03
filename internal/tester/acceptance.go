package tester

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"foundry.dev/foundry/internal/gpt"
	"foundry.dev/foundry/internal/imgutil"
	"foundry.dev/foundry/internal/uki"
)

// Phase 0 acceptance tests (docs/plan/phase-0-foundations.md). Each returns
// a Report; Passed is the verdict.

// Images bundles the inputs of the acceptance suite.
type Images struct {
	Stem        string // <osId>_<version> of v1, e.g. ci-server_1.0.0
	Version     string // v1 version
	Signed      string // v1 signed disk (enrolls keys on first SB boot)
	SignedUKI   string // v1 signed UKI (for the PCR 11 prediction)
	Unsigned    string // v1 unsigned disk (as built)
	UnsignedUKI string // v1 unsigned UKI
	ForeignUKI  string // v1 UKI signed with another tenant's PCR key
	UpdateDir   string // v2 update payload (systemd-sysupdate source files)
	V2Version   string
}

func newReport(name string, m *Machine) *Report {
	return &Report{Scenario: Scenario{Name: name}, Accel: m.Accel}
}

func (r *Report) failf(format string, a ...any) {
	r.Failures = append(r.Failures, fmt.Sprintf(format, a...))
}

func (r *Report) add(s *Session) {
	r.Boots = append(r.Boots, completed(s.Boots)...)
	r.Events = append(r.Events, s.Events...)
	r.Serial = s.Serial
	if s.Failure != "" {
		r.failf("%s", s.Failure)
	}
}

func (r *Report) done(start time.Time) *Report {
	r.Passed = len(r.Failures) == 0
	r.Seconds = time.Since(start).Seconds()
	return r
}

func espOf(disk string) (gpt.Partition, error) {
	t, err := gpt.ReadFile(disk)
	if err != nil {
		return gpt.Partition{}, err
	}
	e := t.ByType(gpt.TypeESP)
	if len(e) != 1 {
		return gpt.Partition{}, fmt.Errorf("expected one ESP, found %d", len(e))
	}
	return e[0], nil
}

// replaceUKI swaps the UKI on the machine's ESP.
func replaceUKI(ctx context.Context, m *Machine, stem, uki string) error {
	esp, err := espOf(m.Disk)
	if err != nil {
		return err
	}
	return imgutil.FAT{Image: m.Disk, Offset: esp.Offset()}.Put(ctx, uki, "/EFI/Linux/"+stem+".efi")
}

// BootMatrix: UEFI and Secure Boot with TPM, each rebooting once
// (acceptance 1, and 4a: unchanged boot unlocks automatically; the running
// system cannot unseal the key).
func BootMatrix(ctx context.Context, c Config, im Images, log io.Writer) []*Report {
	var out []*Report
	for _, sc := range []struct {
		sc    Scenario
		image string
	}{
		{Scenario{Name: "uefi-tpm-reboot", TPM: true, Reboot: true}, im.Unsigned},
		{Scenario{Name: "sb-tpm-reboot", SecureBoot: true, TPM: true, Reboot: true, SBEnforced: true, InitrdOnlyUnseal: true}, im.Signed},
	} {
		s := sc.sc
		s.Version, s.Slot, s.Crypt, s.TPM2 = im.Version, im.Stem, "yes", "yes"
		if s.SecureBoot && im.SignedUKI != "" {
			p, err := uki.PredictPCR11(ctx, "", im.SignedUKI)
			if err != nil {
				out = append(out, &Report{Scenario: s, Failures: []string{"PCR 11 prediction: " + err.Error()}})
				continue
			}
			s.PCR11 = p["enter-initrd:leave-initrd:sysinit:ready"]
		}
		rep, err := Run(ctx, c, sc.image, s, log)
		if err != nil {
			rep = &Report{Scenario: s, Failures: []string{err.Error()}}
		}
		out = append(out, rep)
	}
	return out
}

var refusedRe = regexp.MustCompile(`(?i)security violation|access denied|not signed|verification failed|Error loading`)

// UnsignedRefused: with Secure Boot enforcing, an unsigned UKI on the ESP is
// refused by the boot chain (acceptance 2).
func UnsignedRefused(ctx context.Context, c Config, im Images, log io.Writer) *Report {
	start := time.Now()
	m, err := NewMachine(ctx, c, "sb-unsigned-uki", im.Signed, true, false, true)
	if err != nil {
		return &Report{Scenario: Scenario{Name: "sb-unsigned-uki"}, Failures: []string{err.Error()}}
	}
	defer m.Close()
	r := newReport("sb-unsigned-uki", m)
	if err := replaceUKI(ctx, m, im.Stem, im.UnsignedUKI); err != nil {
		r.failf("prepare: %v", err)
		return r.done(start)
	}
	s, err := m.Boot(ctx, Expect{Match: refusedRe, Forbid: regexp.MustCompile(`Linux version|FOUNDRY-BOOT-OK`), Timeout: 10 * time.Minute, PerBoot: 8 * time.Minute}, log)
	if err != nil {
		r.failf("%v", err)
		return r.done(start)
	}
	r.add(s)
	if s.Matched == "" && s.Failure == "" {
		r.failf("no refusal message on the console")
	} else if s.Matched != "" {
		r.Events = append(r.Events, "refused: "+s.Matched)
	}
	return r.done(start)
}

var verityRe = regexp.MustCompile(`(?i)verity.*(corrupt|error|fail)|data block \d+ is corrupted`)

// VerityCorruption: flipping one byte of the usr partition makes dm-verity
// refuse the block (acceptance 3).
func VerityCorruption(ctx context.Context, c Config, im Images, log io.Writer) *Report {
	start := time.Now()
	m, err := NewMachine(ctx, c, "verity-corruption", im.Unsigned, false, true, true)
	if err != nil {
		return &Report{Scenario: Scenario{Name: "verity-corruption"}, Failures: []string{err.Error()}}
	}
	defer m.Close()
	r := newReport("verity-corruption", m)
	t, err := gpt.ReadFile(m.Disk)
	if err != nil {
		r.failf("%v", err)
		return r.done(start)
	}
	usr, ok := t.ByName(im.Stem)
	if !ok {
		r.failf("no usr partition %s", im.Stem)
		return r.done(start)
	}
	// Byte 1024 of the partition is inside the EROFS superblock (data block 0),
	// which is read when /usr is mounted.
	f, err := os.OpenFile(m.Disk, os.O_RDWR, 0)
	if err != nil {
		r.failf("%v", err)
		return r.done(start)
	}
	b := make([]byte, 1)
	f.ReadAt(b, usr.Offset()+1024)
	b[0] ^= 0xff
	f.WriteAt(b, usr.Offset()+1024)
	f.Close()
	r.Events = append(r.Events, fmt.Sprintf("flipped byte at usr+1024 (disk offset %d)", usr.Offset()+1024))
	s, err := m.Boot(ctx, Expect{Mode: "report", Match: verityRe, Forbid: regexp.MustCompile(`FOUNDRY-BOOT-OK`), Timeout: 12 * time.Minute, PerBoot: 10 * time.Minute}, log)
	if err != nil {
		r.failf("%v", err)
		return r.done(start)
	}
	r.add(s)
	if s.Matched == "" && s.Failure == "" {
		r.failf("no dm-verity corruption message")
	} else if s.Matched != "" {
		r.Events = append(r.Events, "kernel: "+s.Matched)
	}
	return r.done(start)
}

var unlockRefusedRe = regexp.MustCompile(`(?i)please enter passphrase|failed to unseal|couldn't find .*signature|no valid tpm2 token|tpm2 operation failed|failed to activate|failed to start .*cryptography setup for root`)

// TPMWrongKey: the root disk unlocks on an unchanged boot, and does not
// unlock after the UKI is replaced by one whose PCR policy is signed with a
// different key (acceptance 4).
func TPMWrongKey(ctx context.Context, c Config, im Images, log io.Writer) *Report {
	start := time.Now()
	m, err := NewMachine(ctx, c, "tpm-foreign-pcr-key", im.Signed, false, true, true)
	if err != nil {
		return &Report{Scenario: Scenario{Name: "tpm-foreign-pcr-key"}, Failures: []string{err.Error()}}
	}
	defer m.Close()
	r := newReport("tpm-foreign-pcr-key", m)
	s, err := m.Boot(ctx, Expect{Mode: "report", Reports: 1, Forbid: panicRe}, log)
	if err != nil {
		r.failf("%v", err)
		return r.done(start)
	}
	r.add(s)
	r.Failures = append(r.Failures, check(Scenario{Version: im.Version, Slot: im.Stem, Crypt: "yes", TPM2: "yes", InitrdOnlyUnseal: true}, completed(s.Boots))...)
	if len(r.Failures) > 0 {
		return r.done(start)
	}
	r.Events = append(r.Events, "unchanged boot: root unlocked by TPM2; unseal from running system refused")
	if err := replaceUKI(ctx, m, im.Stem, im.ForeignUKI); err != nil {
		r.failf("replace UKI: %v", err)
		return r.done(start)
	}
	s, err = m.Boot(ctx, Expect{Mode: "report", Match: unlockRefusedRe, Forbid: regexp.MustCompile(`FOUNDRY-BOOT-OK`), Timeout: 12 * time.Minute, PerBoot: 10 * time.Minute}, log)
	if err != nil {
		r.failf("%v", err)
		return r.done(start)
	}
	r.add(s)
	if s.Matched == "" && s.Failure == "" {
		r.failf("root unlocked (or no unlock failure seen) with a foreign PCR signing key")
	} else if s.Matched != "" {
		r.Events = append(r.Events, "unlock refused: "+s.Matched)
	}
	return r.done(start)
}

// ABFallback: install v2 into slot B with systemd-sysupdate, boot it as a
// counted trial (+3) whose health check fails, and require the automatic
// fallback to v1 (acceptance 5).
func ABFallback(ctx context.Context, c Config, im Images, log io.Writer) *Report {
	start := time.Now()
	c.ExtraDiskGiB = 6
	m, err := NewMachine(ctx, c, "ab-fallback", im.Signed, false, true, false)
	if err != nil {
		return &Report{Scenario: Scenario{Name: "ab-fallback"}, Failures: []string{err.Error()}}
	}
	defer m.Close()
	r := newReport("ab-fallback", m)
	upd := filepath.Join(m.dir, "update.ext4")
	if err := makeUpdateDisk(ctx, im.UpdateDir, upd); err != nil {
		r.failf("update medium: %v", err)
		return r.done(start)
	}
	m.AddDisk(upd)
	s, err := m.Boot(ctx, Expect{Mode: "abtest", Reports: 1, Forbid: panicRe, Timeout: 8 * c.BootTimeout, PerBoot: c.BootTimeout}, log)
	if err != nil {
		r.failf("%v", err)
		return r.done(start)
	}
	r.add(s)
	var updated int
	for _, e := range s.Events {
		if strings.HasPrefix(e, "FOUNDRY-UPDATED") {
			updated++
		}
	}
	// Count kernels started with v2's /usr (usrhash prefix = usr partition UUID).
	v2prefix := ""
	if ents, err := os.ReadDir(im.UpdateDir); err == nil {
		for _, e := range ents {
			if n := e.Name(); strings.HasSuffix(n, ".usr.raw") {
				u := strings.TrimSuffix(n[strings.LastIndex(n, "_")+1:], ".usr.raw")
				v2prefix = strings.ReplaceAll(u, "-", "")
			}
		}
	}
	trials := 0
	for _, k := range s.Kernels {
		if v2prefix != "" && strings.HasPrefix(k, v2prefix) {
			trials++
		}
	}
	if trials < 3 {
		r.failf("expected 3 counted trial boots of %s before fallback, saw %d", im.V2Version, trials)
	}
	if updated != 1 {
		r.failf("update to %s was not installed (%d update events)", im.V2Version, updated)
	}
	boots := completed(s.Boots)
	if len(boots) == 0 {
		r.failf("no final report")
		return r.done(start)
	}
	last := boots[len(boots)-1].Fields
	if last["version"] != im.Version {
		r.failf("final boot runs version %q, want fallback to %q", last["version"], im.Version)
	}
	if strings.Contains(last["entry"], im.V2Version) {
		r.failf("final boot entry %q is the failed update", last["entry"])
	}
	r.Events = append(r.Events, fmt.Sprintf("update %s installed; %d trial boots of it failed the health check; fell back to %s (entry %s)",
		im.V2Version, trials, last["version"], last["entry"]))
	return r.done(start)
}

func makeUpdateDisk(ctx context.Context, dir, img string) error {
	var total int64
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if st, err := e.Info(); err == nil {
			total += st.Size()
		}
	}
	size := total + total/8 + 64<<20
	if out, err := exec.CommandContext(ctx, "mkfs.ext4", "-q", "-F", "-L", "FOUNDRYUPD", "-d", dir, img, fmt.Sprintf("%dk", size/1024)).CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.ext4: %v: %s", err, out)
	}
	return nil
}

// Acceptance runs the whole Phase 0 suite.
func Acceptance(ctx context.Context, c Config, im Images, only map[string]bool, log io.Writer) []*Report {
	var out []*Report
	want := func(n string) bool { return len(only) == 0 || only[n] }
	if want("matrix") {
		out = append(out, BootMatrix(ctx, c, im, log)...)
	}
	if want("unsigned") {
		out = append(out, UnsignedRefused(ctx, c, im, log))
	}
	if want("verity") {
		out = append(out, VerityCorruption(ctx, c, im, log))
	}
	if want("tpm") {
		out = append(out, TPMWrongKey(ctx, c, im, log))
	}
	if want("ab") {
		out = append(out, ABFallback(ctx, c, im, log))
	}
	return out
}
