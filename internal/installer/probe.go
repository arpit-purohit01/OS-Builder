package installer

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"foundry.dev/foundry/internal/gpt"
)

// Sys abstracts the host for probing (tests point it at fixtures).
type Sys struct {
	SysBlock string // /sys/block
	Dev      string // /dev
	EFIVars  string // /sys/firmware/efi/efivars
	ByUUID   string // /dev/disk/by-partuuid
}

var Host = Sys{SysBlock: "/sys/block", Dev: "/dev", EFIVars: "/sys/firmware/efi/efivars", ByUUID: "/dev/disk/by-partuuid"}

const loaderGUID = "4a67b082-0a4c-41cf-b6c7-440b29bb8c4f"

func readTrim(p string) string {
	b, _ := os.ReadFile(p)
	return strings.TrimSpace(string(b))
}

// BootESP returns the PARTUUID of the ESP systemd-boot was started from.
func (s Sys) BootESP() (string, error) {
	b, err := os.ReadFile(filepath.Join(s.EFIVars, "LoaderDevicePartUUID-"+loaderGUID))
	if err != nil {
		return "", fmt.Errorf("installer: not booted through systemd-boot: %w", err)
	}
	if len(b) < 4 {
		return "", errors.New("installer: empty LoaderDevicePartUUID")
	}
	u := make([]uint16, 0, len(b[4:])/2)
	for i := 4; i+1 < len(b); i += 2 {
		if c := binary.LittleEndian.Uint16(b[i:]); c != 0 {
			u = append(u, c)
		}
	}
	return strings.ToLower(string(utf16Decode(u))), nil
}

func utf16Decode(u []uint16) []rune {
	r := make([]rune, len(u))
	for i, c := range u {
		r[i] = rune(c)
	}
	return r
}

// DiskOfPartUUID resolves a partition UUID to its whole-disk device.
func (s Sys) DiskOfPartUUID(uuid string) (string, error) {
	part, err := filepath.EvalSymlinks(filepath.Join(s.ByUUID, uuid))
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Join(s.SysBlock, "..", "class", "block", filepath.Base(part), ".."))
	if err != nil {
		return "", err
	}
	return filepath.Join(s.Dev, filepath.Base(parent)), nil
}

// PartDev returns the device node of partition number n on disk.
func PartDev(disk string, n int) string {
	base := filepath.Base(disk)
	if c := base[len(base)-1]; c >= '0' && c <= '9' {
		return disk + "p" + strconv.Itoa(n)
	}
	return disk + strconv.Itoa(n)
}

// Disks lists installable whole disks.
func (s Sys) Disks(sourcePath string) ([]Disk, error) {
	ents, err := os.ReadDir(s.SysBlock)
	if err != nil {
		return nil, err
	}
	var out []Disk
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, "loop") || strings.HasPrefix(n, "ram") || strings.HasPrefix(n, "zram") || strings.HasPrefix(n, "dm-") || strings.HasPrefix(n, "sr") || strings.HasPrefix(n, "md") {
			continue
		}
		dir := filepath.Join(s.SysBlock, n)
		sectors, _ := strconv.ParseInt(readTrim(filepath.Join(dir, "size")), 10, 64)
		if sectors == 0 || readTrim(filepath.Join(dir, "ro")) == "1" {
			continue
		}
		d := Disk{Path: filepath.Join(s.Dev, n), Size: sectors * 512, Removable: readTrim(filepath.Join(dir, "removable")) == "1",
			Model: readTrim(filepath.Join(dir, "device", "model")), Serial: readTrim(filepath.Join(dir, "device", "serial"))}
		if d.Serial == "" {
			d.Serial = readTrim(filepath.Join(dir, "serial"))
		}
		if strings.HasPrefix(n, "nvme") {
			d.Transport = "nvme"
		} else if strings.HasPrefix(n, "vd") {
			d.Transport = "virtio"
		}
		d.IsSource = d.Path == sourcePath
		if err := ProbeDisk(&d); err != nil {
			d.Foreign = true
		}
		out = append(out, d)
	}
	return out, nil
}

// ProbeDisk reads the partition table and identifies file systems.
func ProbeDisk(d *Disk) error {
	f, err := os.Open(d.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	t, err := gpt.Read(f)
	if err != nil {
		mbr := make([]byte, 512)
		if _, e := f.ReadAt(mbr, 0); e == nil && mbr[510] == 0x55 && mbr[511] == 0xAA && !bytes.Equal(mbr[446:510], make([]byte, 64)) {
			d.Foreign = true // MBR-partitioned
			return nil
		}
		return nil // empty disk
	}
	d.Table = t
	d.Parts = nil
	for _, p := range t.Partitions {
		pi := PartInfo{Partition: p, FS: sniffFS(f, p.Offset())}
		if pi.FS == "bitlocker" {
			d.BitLocker = true
		}
		d.Parts = append(d.Parts, pi)
	}
	return nil
}

// sniffFS identifies a file system by its superblock magic.
func sniffFS(f *os.File, off int64) string {
	b := make([]byte, 2048)
	if _, err := f.ReadAt(b, off); err != nil {
		return ""
	}
	switch {
	case string(b[3:11]) == "-FVE-FS-":
		return "bitlocker"
	case string(b[3:11]) == "NTFS    ":
		return "ntfs"
	case string(b[82:87]) == "FAT32" || string(b[54:57]) == "FAT":
		return "vfat"
	case binary.LittleEndian.Uint16(b[1080:]) == 0xEF53:
		return "ext4"
	case string(b[1024:1028]) == "\xe2\xe1\xf5\xe0":
		return "erofs"
	}
	return ""
}

// ProbeESP mounts a disk's ESP read-only to find Windows and free space.
func ProbeESP(d *Disk, r Runner) error {
	for _, p := range d.Parts {
		if p.Type != gpt.TypeESP {
			continue
		}
		mnt, err := os.MkdirTemp("", "foundry-esp-")
		if err != nil {
			return err
		}
		defer os.Remove(mnt)
		if err := r.Run("mount", "-o", "ro", "-t", "vfat", PartDev(d.Path, p.Index), mnt); err != nil {
			return fmt.Errorf("installer: cannot read the EFI system partition: %w", err)
		}
		defer r.Run("umount", mnt)
		if _, err := os.Stat(filepath.Join(mnt, "EFI", "Microsoft", "Boot", "bootmgfw.efi")); err == nil {
			d.WindowsBoot = true
		}
		var st syscall.Statfs_t
		if err := syscall.Statfs(mnt, &st); err == nil {
			d.ESPFree = int64(st.Bavail) * int64(st.Bsize)
		}
		return nil
	}
	return nil
}
