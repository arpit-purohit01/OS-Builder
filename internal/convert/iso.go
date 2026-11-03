package convert

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"foundry.dev/foundry/internal/gpt"
	"foundry.dev/foundry/internal/imgutil"
)

// ISO builds a hybrid ISO from the signed disk: the ESP (also the El Torito
// EFI boot image) and slot A's usr, usr-verity and usr-verity-sig are
// appended as GPT partitions with their original type, UUID and label, so
// usrhash= finds /usr exactly as on the disk image. Written to a USB stick it
// is a GPT disk; booted as a CD the initrd attaches it to a partition-scanning
// loop device. The installer entry is the default (live media install).
func ISO(ctx context.Context, disk, out, volid string, epoch time.Time) error {
	t, err := gpt.ReadFile(disk)
	if err != nil {
		return err
	}
	var esp, usr, ver, sig *gpt.Partition
	for i := range t.Partitions {
		p := &t.Partitions[i]
		switch {
		case p.Type == gpt.TypeESP:
			esp = p
		case isUsr(p.Type) && p.Name != "_empty":
			usr = p
		case isVerity(p.Type) && p.Name != "_empty":
			ver = p
		case isSig(p.Type) && p.Name != "_empty":
			sig = p
		}
	}
	if esp == nil || usr == nil || ver == nil || sig == nil {
		return errors.New("convert: disk lacks ESP or slot A partitions")
	}
	dir, err := os.MkdirTemp(filepath.Dir(out), "iso-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	parts := []*gpt.Partition{esp, usr, ver, sig}
	files := make([]string, len(parts))
	for i, p := range parts {
		files[i] = filepath.Join(dir, fmt.Sprintf("p%d.img", i))
		if err := imgutil.ExtractRange(disk, p.Offset(), p.Size(), files[i]); err != nil {
			return err
		}
	}
	// Live media boot the installer by default; the OS entry stays in the menu.
	fat := imgutil.FAT{Image: files[0], MTime: epoch}
	if err := fat.PutBytes(ctx, []byte("timeout 3\nconsole-mode keep\neditor no\ndefault *-installer_*\n"), "/loader/loader.conf"); err != nil {
		return err
	}
	root := filepath.Join(dir, "root")
	os.MkdirAll(root, 0o755)
	os.WriteFile(filepath.Join(root, "README.TXT"), []byte(volid+" installation medium.\r\n\r\nWrite this file to a USB stick (balenaEtcher, Rufus in DD mode, or dd),\r\nor boot it as a DVD/virtual CD, then follow the installer.\r\n"), 0o644)
	vol := strings.ToUpper(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			return r
		}
		return '_'
	}, volid))
	if len(vol) > 32 {
		vol = vol[:32]
	}
	args := []string{"-as", "mkisofs", "-o", out, "-V", vol, "-iso-level", "3", "-J", "-R",
		"--modification-date=" + epoch.UTC().Format("2006010215040500"),
		"--gpt_disk_guid", gpt.GUIDFrom("foundry-iso", usr.UUID.String()).String(),
		"-appended_part_as_gpt"}
	for i, p := range parts {
		args = append(args, "-append_partition", fmt.Sprint(i+1), p.Type.String(), files[i])
	}
	args = append(args, "-e", "--interval:appended_partition_1:all::", "-no-emul-boot", root)
	env := []string{"SOURCE_DATE_EPOCH=" + fmt.Sprint(epoch.Unix())}
	if err := imgutil.Run(ctx, env, "xorriso", args...); err != nil {
		return err
	}
	// Give the appended partitions their original identity.
	it, err := gpt.ReadFile(out)
	if err != nil {
		return fmt.Errorf("convert: ISO has no readable GPT: %w", err)
	}
	for _, p := range parts {
		var match *gpt.Partition
		for i := range it.Partitions {
			q := &it.Partitions[i]
			if q.Type == p.Type && q.Size() >= p.Size() {
				match = q
				break
			}
		}
		if match == nil {
			return fmt.Errorf("convert: appended partition %s missing from the ISO", p.Name)
		}
		fixed := *match
		fixed.UUID, fixed.Name, fixed.Attrs = p.UUID, p.Name, p.Attrs
		if p.Type == gpt.TypeESP {
			fixed.Name = "ESP"
		}
		if err := it.Set(fixed); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(out, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return it.Write(f)
}

func isUsr(g gpt.GUID) bool    { return g == gpt.TypeUsrX86_64 || g == gpt.TypeUsrArm64 }
func isVerity(g gpt.GUID) bool { return g == gpt.TypeUsrVerityX86_64 || g == gpt.TypeUsrVerityArm64 }
func isSig(g gpt.GUID) bool    { return g == gpt.TypeUsrVeritySigX8664 || g == gpt.TypeUsrVeritySigArm64 }
