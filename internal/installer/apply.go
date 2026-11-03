package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"foundry.dev/foundry/internal/gpt"
)

// Runner runs host commands (mount, mkfs, udevadm).
type Runner interface {
	Run(name string, args ...string) error
}

type ExecRunner struct{}

func (ExecRunner) Run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Progress receives human-readable steps and copy progress (0..1).
type Progress func(step string, frac float64)

// Record is written to the ESP as /loader/foundry-install.json.
type Record struct {
	Installer string    `json:"installer"`
	Mode      Mode      `json:"mode"`
	Time      time.Time `json:"time"`
	OSID      string    `json:"osId"`
	Target    string    `json:"target"`
	BootEntry string    `json:"bootEntry,omitempty"`
}

const blk = 4 << 20

// Apply executes a plan. It refuses targets with mounted partitions.
func Apply(ctx context.Context, src Source, pl *Plan, sys Sys, r Runner, version string, progress Progress) error {
	if progress == nil {
		progress = func(string, float64) {}
	}
	if err := notMounted(pl.Target); err != nil {
		return err
	}
	in, err := os.Open(src.Path)
	if err != nil {
		return err
	}
	defer in.Close()
	// O_EXCL on a block device fails if anything holds it (mounts, dm).
	out, err := os.OpenFile(pl.Target, os.O_RDWR|syscall.O_EXCL, 0)
	if err != nil {
		return fmt.Errorf("installer: open %s exclusively: %w", pl.Target, err)
	}
	defer out.Close()

	progress("Writing the partition table", 0)
	if pl.Fresh {
		// Wipe old signatures at both ends (stale GPT backups confuse firmware).
		zero := make([]byte, 1<<20)
		out.WriteAt(zero, 0)
		out.WriteAt(zero, pl.TargetSize-int64(len(zero)))
		if err := gpt.WriteProtectiveMBR(out, pl.TargetSize); err != nil {
			return err
		}
	}
	if err := pl.Table.Write(out); err != nil {
		return err
	}

	var total, done int64
	for _, c := range pl.Copies {
		total += c.Src.Size()
	}
	buf := make([]byte, blk)
	for _, c := range pl.Copies {
		name := c.Src.Name
		if name == "" || name == "_empty" {
			name = "empty update slot"
		}
		progress("Copying "+name, float64(done)/float64(total))
		for off := int64(0); off < c.Src.Size(); off += blk {
			if err := ctx.Err(); err != nil {
				return err
			}
			n := min(int64(blk), c.Src.Size()-off)
			if _, err := in.ReadAt(buf[:n], c.Src.Offset()+off); err != nil && !errors.Is(err, io.EOF) {
				return fmt.Errorf("installer: read %s: %w", name, err)
			}
			if allZero(buf[:n]) {
				if err := zeroRange(out, c.Dst.Offset()+off, n); err != nil {
					return fmt.Errorf("installer: zero %s: %w", name, err)
				}
			} else if _, err := out.WriteAt(buf[:n], c.Dst.Offset()+off); err != nil {
				return fmt.Errorf("installer: write %s: %w", name, err)
			}
			done += n
			if off%(64*blk) == 0 {
				progress("Copying "+name, float64(done)/float64(total))
			}
		}
	}
	// Empty update slots: signature partitions must read as zeros (no stale
	// data that could look like a signature); data slots are overwritten by
	// the first update before use.
	for _, z := range pl.Zero {
		if err := zeroRange(out, z.Offset(), z.Size()); err != nil {
			return err
		}
	}
	progress("Flushing to disk", 1)
	if err := out.Sync(); err != nil {
		return err
	}
	// Verify what we copied before touching boot configuration.
	if err := verifyCopies(in, out, pl.Copies); err != nil {
		return err
	}
	out.Close()
	r.Run("blockdev", "--rereadpt", pl.Target)
	r.Run("udevadm", "settle", "--timeout=30")

	progress("Setting up the boot loader", 1)
	rec := Record{Installer: version, Mode: pl.Mode, Time: time.Now().UTC(), OSID: src.OSID, Target: pl.Target}
	if err := configureBoot(src, pl, r, &rec); err != nil {
		return err
	}
	if _, err := os.Stat(sys.EFIVars); err == nil {
		esp := pl.ESP
		if p, ok := pl.Table.ByName(esp.Name); ok && esp.Index == 0 {
			esp = p
		}
		loader := "/EFI/systemd/systemd-boot" + efiArch(src.Arch) + ".efi"
		if n, err := AddBootEntry(sys.EFIVars, osName(src), esp, loader, pl.BootFirst); err == nil {
			rec.BootEntry = n
		} else {
			progress("Note: could not add a firmware boot entry ("+err.Error()+"); the disk still boots through the default path", 1)
		}
	}
	progress("Done", 1)
	return nil
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

// zeroRange zeroes a byte range: BLKZEROOUT on block devices (write-zeroes or
// unmap on the drive, sparse on virtual disks), plain writes otherwise.
func zeroRange(f *os.File, off, n int64) error {
	r := [2]uint64{uint64(off), uint64(n)}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), 0x127f, uintptr(unsafe.Pointer(&r))); e == 0 { // BLKZEROOUT
		return nil
	}
	z := make([]byte, min(n, blk))
	for done := int64(0); done < n; {
		k := min(int64(len(z)), n-done)
		if _, err := f.WriteAt(z[:k], off+done); err != nil {
			return err
		}
		done += k
	}
	return nil
}

func osName(src Source) string {
	if src.OSID == "" {
		return "Foundry OS"
	}
	return src.OSID
}

func efiArch(a string) string {
	if a == "aarch64" {
		return "aa64"
	}
	return "x64"
}

// verifyCopies re-reads the first and last 4 MiB of every copied partition.
func verifyCopies(in, out *os.File, copies []Copy) error {
	a, b := make([]byte, blk), make([]byte, blk)
	for _, c := range copies {
		for _, off := range []int64{0, max(0, c.Src.Size()-blk)} {
			n := min(int64(blk), c.Src.Size())
			if _, err := in.ReadAt(a[:n], c.Src.Offset()+off); err != nil {
				return err
			}
			if _, err := out.ReadAt(b[:n], c.Dst.Offset()+off); err != nil {
				return err
			}
			if string(a[:n]) != string(b[:n]) {
				return fmt.Errorf("installer: verification of %q failed: the target disk may be faulty", c.Src.Name)
			}
		}
	}
	return nil
}

func configureBoot(src Source, pl *Plan, r Runner, rec *Record) error {
	espDev := PartDev(pl.Target, pl.ESP.Index)
	mnt, err := os.MkdirTemp("", "foundry-target-esp-")
	if err != nil {
		return err
	}
	defer os.Remove(mnt)
	if err := r.Run("mount", "-t", "vfat", espDev, mnt); err != nil {
		return err
	}
	defer r.Run("umount", mnt)
	switch pl.Mode {
	case Erase:
		// The installer entry stays on the medium only.
		inst, _ := filepath.Glob(filepath.Join(mnt, "EFI", "Linux", "*-installer_*.efi"))
		for _, p := range inst {
			if err := os.Remove(p); err != nil {
				return err
			}
		}
	case Alongside:
		srcMnt, err := os.MkdirTemp("", "foundry-src-esp-")
		if err != nil {
			return err
		}
		defer os.Remove(srcMnt)
		srcESP, ok := espOf(src.Table)
		if !ok {
			return errors.New("installer: medium has no ESP")
		}
		if err := r.Run("mount", "-o", "ro", "-t", "vfat", PartDev(src.Path, srcESP.Index), srcMnt); err != nil {
			return err
		}
		defer r.Run("umount", srcMnt)
		xb := PartDev(pl.Target, pl.Xbootldr.Index)
		if err := r.Run("mkfs.vfat", "-F", "32", "-n", "XBOOTLDR", xb); err != nil {
			return err
		}
		xbMnt, err := os.MkdirTemp("", "foundry-xbootldr-")
		if err != nil {
			return err
		}
		defer os.Remove(xbMnt)
		if err := r.Run("mount", "-t", "vfat", xb, xbMnt); err != nil {
			return err
		}
		defer r.Run("umount", xbMnt)
		bl := "systemd-boot" + efiArch(src.Arch) + ".efi"
		if err := copyFile(filepath.Join(srcMnt, "EFI", "systemd", bl), filepath.Join(mnt, "EFI", "systemd", bl)); err != nil {
			return err
		}
		// Never replace Windows' fallback loader.
		fb := filepath.Join(mnt, "EFI", "BOOT", "BOOT"+strings.ToUpper(efiArch(src.Arch))+".EFI")
		if _, err := os.Stat(fb); os.IsNotExist(err) {
			if err := copyFile(filepath.Join(srcMnt, "EFI", "systemd", bl), fb); err != nil {
				return err
			}
		}
		ukis, _ := filepath.Glob(filepath.Join(srcMnt, "EFI", "Linux", src.OSID+"_*.efi"))
		if len(ukis) == 0 {
			return errors.New("installer: no kernel image on the medium")
		}
		for _, u := range ukis {
			if err := copyFile(u, filepath.Join(xbMnt, "EFI", "Linux", filepath.Base(u))); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(filepath.Join(mnt, "loader"), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(mnt, "loader", "loader.conf"), []byte(pl.Loader), 0o644); err != nil {
			return err
		}
	}
	b, _ := json.MarshalIndent(rec, "", "  ")
	os.MkdirAll(filepath.Join(mnt, "loader"), 0o755)
	return os.WriteFile(filepath.Join(mnt, "loader", "foundry-install.json"), b, 0o644)
}

func espOf(t *gpt.Table) (gpt.Partition, bool) {
	e := t.ByType(gpt.TypeESP)
	if len(e) != 1 {
		return gpt.Partition{}, false
	}
	return e[0], true
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// notMounted refuses a target that has mounted partitions.
func notMounted(disk string) error {
	b, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return nil
	}
	base := filepath.Base(disk)
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) > 1 && strings.HasPrefix(filepath.Base(f[0]), base) && strings.HasPrefix(f[0], "/dev/") {
			return fmt.Errorf("installer: %s is in use (%s mounted on %s)", disk, f[0], f[1])
		}
	}
	return nil
}
