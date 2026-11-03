package tester

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"foundry.dev/foundry/internal/gpt"
	"foundry.dev/foundry/internal/imgutil"
)

// Installer acceptance (Phase 2, acceptance 3): boot the signed medium's
// installer entry with an unattended config, then boot the target disk on
// its own (same NVRAM) and require a healthy, encrypted first boot.
//
// The Windows fixture is synthetic: GPT with an ESP holding Windows Boot
// Manager files, MSR, an NTFS (or BitLocker) C: volume with marker data, a
// recovery partition, and unallocated space. Bytes of every Windows
// partition are hashed before and after installing.

const gibSectors = (1 << 30) / 512

var (
	installOKRe   = regexp.MustCompile(`FOUNDRY-INSTALL-OK`)
	installFailRe = regexp.MustCompile(`FOUNDRY-INSTALL-FAILED[^\r\n]*`)
)

// WindowsFixture writes a sparse disk resembling a Windows install.
func WindowsFixture(ctx context.Context, path string, size int64, freeGiB int, bitlocker bool) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		return err
	}
	t, err := gpt.New(size, gpt.GUIDFrom("fixture-windows"))
	if err != nil {
		return err
	}
	esp, _ := t.Add(gpt.Partition{Type: gpt.TypeESP, UUID: gpt.GUIDFrom("win-esp"), FirstLBA: 2048, LastLBA: 2048 + 100*2048 - 1, Name: "EFI system partition"})
	msr, _ := t.Add(gpt.Partition{Type: gpt.TypeMSReserved, UUID: gpt.GUIDFrom("win-msr"), FirstLBA: esp.LastLBA + 1, LastLBA: esp.LastLBA + 16*2048, Name: "Microsoft reserved partition"})
	recFirst := t.LastUsable() - gibSectors + 1
	cLast := recFirst - 1 - uint64(freeGiB)*gibSectors
	c, _ := t.Add(gpt.Partition{Type: gpt.TypeMSBasicData, UUID: gpt.GUIDFrom("win-c"), FirstLBA: msr.LastLBA + 1, LastLBA: cLast, Name: "Basic data partition"})
	rec, _ := t.Add(gpt.Partition{Type: gpt.TypeWinRecovery, UUID: gpt.GUIDFrom("win-recovery"), FirstLBA: recFirst, LastLBA: t.LastUsable()})
	if err := gpt.WriteProtectiveMBR(f, size); err != nil {
		return err
	}
	if err := t.Write(f); err != nil {
		return err
	}
	sig := "NTFS    "
	if bitlocker {
		sig = "-FVE-FS-"
	}
	for _, p := range []gpt.Partition{c, rec} {
		boot := make([]byte, 512)
		copy(boot[3:], sig)
		boot[510], boot[511] = 0x55, 0xAA
		f.WriteAt(boot, p.Offset())
		// Marker data spread over the volume (checked byte-for-byte later).
		for i := int64(1); i <= 8; i++ {
			mark := bytes.Repeat([]byte(fmt.Sprintf("WINDOWS-DATA-%s-%d|", p.Name, i)), 64)
			f.WriteAt(mark, p.Offset()+p.Size()*i/9)
		}
	}
	f.Close()
	fat := imgutil.FAT{Image: path, Offset: esp.Offset()}
	if err := imgutil.Run(ctx, []string{"MTOOLS_SKIP_CHECK=1"}, "mformat", "-i", fmt.Sprintf("%s@@%d", path, esp.Offset()), "-F", "-v", "SYSTEM", "-T", fmt.Sprint(esp.LastLBA-esp.FirstLBA+1), "::"); err != nil {
		return err
	}
	for _, n := range []string{"/EFI/Microsoft/Boot/bootmgfw.efi", "/EFI/Microsoft/Boot/BCD", "/EFI/BOOT/BOOTX64.EFI"} {
		if err := fat.PutBytes(ctx, []byte("windows boot manager fixture: "+n), n); err != nil {
			return err
		}
	}
	return nil
}

// partHashes hashes every partition except the ESP (which the installer
// legitimately writes to in dual-boot mode).
func partHashes(path string, only map[gpt.GUID]bool) (map[string]string, error) {
	t, err := gpt.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	for _, p := range t.Partitions {
		if !only[p.Type] {
			continue
		}
		h := sha256.New()
		if _, err := io.Copy(h, io.NewSectionReader(f, p.Offset(), p.Size())); err != nil {
			return nil, err
		}
		out[p.UUID.String()] = fmt.Sprintf("%x", h.Sum(nil))
	}
	return out, nil
}

// InstallScenario runs one installer scenario: "install-erase",
// "install-alongside" or "install-bitlocker-refused".
func InstallScenario(ctx context.Context, c Config, signed, name string, log io.Writer) *Report {
	start := time.Now()
	r := &Report{Scenario: Scenario{Name: name, TPM: true}}
	fail := func(format string, a ...any) *Report {
		r.failf(format, a...)
		return r.done(start)
	}
	live, err := NewMachine(ctx, c, name+"-live", signed, false, false, true)
	if err != nil {
		return fail("%v", err)
	}
	defer live.Close()
	r.Accel = live.Accel
	esp, err := espOf(live.Disk)
	if err != nil {
		return fail("%v", err)
	}
	// Choose the installer entry on the medium (loader.conf is not signed).
	if err := (imgutil.FAT{Image: live.Disk, Offset: esp.Offset()}).PutBytes(ctx, []byte("timeout 0\nconsole-mode keep\neditor no\ndefault *-installer_*\n"), "/loader/loader.conf"); err != nil {
		return fail("select installer entry: %v", err)
	}
	target := filepath.Join(live.dir, "target.raw")
	cfg := map[string]any{"target": "auto", "after": "poweroff", "makeDefault": true}
	windows := map[gpt.GUID]bool{gpt.TypeMSReserved: true, gpt.TypeMSBasicData: true, gpt.TypeWinRecovery: true}
	var before map[string]string
	switch name {
	case "install-erase":
		f, err := os.Create(target)
		if err != nil {
			return fail("%v", err)
		}
		f.Truncate(48 << 30)
		f.Close()
		cfg["mode"] = "erase"
	case "install-alongside", "install-bitlocker-refused":
		if err := WindowsFixture(ctx, target, 120<<30, 60, name == "install-bitlocker-refused"); err != nil {
			return fail("windows fixture: %v", err)
		}
		if before, err = partHashes(target, windows); err != nil {
			return fail("%v", err)
		}
		cfg["mode"] = "alongside"
	default:
		return fail("unknown install scenario %q", name)
	}
	live.AddWritableDisk(target)
	b, _ := json.Marshal(cfg)
	live.Credentials = map[string][]byte{"foundry.install": b}

	expectFail := name == "install-bitlocker-refused"
	s, err := live.Boot(ctx, Expect{Match: installOKRe, Forbid: regexp.MustCompile(installFailRe.String() + `|Kernel panic|emergency mode`),
		Timeout: 40 * time.Minute, PerBoot: 25 * time.Minute}, log)
	if err != nil {
		return fail("%v", err)
	}
	r.Serial = s.Serial
	if expectFail {
		if s.Failure == "" || !regexp.MustCompile(`BitLocker`).MatchString(s.Failure) {
			return fail("installer did not refuse BitLocker without confirmation (matched %q, failure %q)", s.Matched, s.Failure)
		}
		r.Events = append(r.Events, "refused: BitLocker confirmation required")
		after, _ := partHashes(target, windows)
		if fmt.Sprint(after) != fmt.Sprint(before) {
			r.failf("Windows partitions changed although the install was refused")
		}
		return r.done(start)
	}
	if s.Matched == "" {
		return fail("installer did not finish: %s", s.Failure)
	}
	r.Events = append(r.Events, s.Matched)
	// Let QEMU finish the poweroff so the target is fully written.
	time.Sleep(2 * time.Second)

	if before != nil {
		after, err := partHashes(target, windows)
		if err != nil {
			return fail("%v", err)
		}
		if fmt.Sprint(after) != fmt.Sprint(before) {
			return fail("Windows partitions were modified")
		}
		r.Events = append(r.Events, fmt.Sprintf("windows partitions unchanged (%d hashed)", len(after)))
		t, err := gpt.ReadFile(target)
		if err != nil {
			return fail("target GPT: %v", err)
		}
		wesp, _ := espOfTable(t)
		fat := imgutil.FAT{Image: target, Offset: wesp.Offset()}
		if !fat.Exists(ctx, "/EFI/Microsoft/Boot/bootmgfw.efi") || !fat.Exists(ctx, "/EFI/systemd/systemd-bootx64.efi") {
			return fail("ESP lost Windows Boot Manager or lacks systemd-boot")
		}
		if fb, _ := fat.Get(ctx, "/EFI/BOOT/BOOTX64.EFI"); !bytes.HasPrefix(fb, []byte("windows boot manager fixture")) {
			return fail("Windows fallback loader was replaced")
		}
		if len(t.ByType(gpt.TypeXBOOTLDR)) != 1 {
			return fail("no XBOOTLDR partition")
		}
	}

	// Boot the installed disk alone, with the NVRAM the installer wrote.
	cc := c
	cc.ExtraDiskGiB = 0
	inst, err := NewMachine(ctx, cc, name+"-installed", target, false, true, false)
	if err != nil {
		return fail("%v", err)
	}
	defer inst.Close()
	if err := inst.UseVars(live.Vars()); err != nil {
		return fail("%v", err)
	}
	s2, err := inst.Boot(ctx, Expect{Mode: "report", Reports: 1, Forbid: panicRe}, log)
	if err != nil {
		return fail("%v", err)
	}
	r.add(s2)
	sc := Scenario{Name: name, TPM: true, Crypt: "yes", TPM2: "yes"}
	r.Failures = append(r.Failures, check(sc, completed(s2.Boots))...)
	return r.done(start)
}

func espOfTable(t *gpt.Table) (gpt.Partition, bool) {
	e := t.ByType(gpt.TypeESP)
	if len(e) != 1 {
		return gpt.Partition{}, false
	}
	return e[0], true
}
