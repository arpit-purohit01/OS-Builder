package installer

import (
	"errors"
	"strings"
	"testing"

	"foundry.dev/foundry/internal/gpt"
)

// medium mimics a finalized image: ESP, usr/verity/sig A, empty B slots.
func medium(t *testing.T) Source {
	tb, err := gpt.New(8*GiB, gpt.GUIDFrom("medium"))
	if err != nil {
		t.Fatal(err)
	}
	lba := uint64(2048)
	add := func(typ gpt.GUID, name string, size int64) {
		n := uint64(size / sector)
		if _, err := tb.Add(gpt.Partition{Type: typ, UUID: gpt.GUIDFrom(name, typ.String(), string(rune(lba))), FirstLBA: lba, LastLBA: lba + n - 1, Name: name}); err != nil {
			t.Fatal(err)
		}
		lba += n
	}
	add(gpt.TypeESP, "esp", 1*GiB)
	add(gpt.TypeUsrX86_64, "aurora_1.0.0", 3*GiB)
	add(gpt.TypeUsrVerityX86_64, "aurora_1.0.0_v", 64*MiB)
	add(gpt.TypeUsrVeritySigX8664, "aurora_1.0.0_s", 16<<10)
	add(gpt.TypeUsrX86_64, "_empty", 3*GiB)
	add(gpt.TypeUsrVerityX86_64, "_empty", 64*MiB)
	add(gpt.TypeUsrVeritySigX8664, "_empty", 16<<10)
	return Source{Path: "/dev/sdz", Size: 8 * GiB, Table: tb, OSID: "aurora", Arch: "x86_64"}
}

func blank(size int64) Disk { return Disk{Path: "/dev/nvme0n1", Size: size} }

// windows builds a typical OEM layout: ESP, MSR, C:, recovery at the end,
// with `free` bytes unallocated between C: and recovery.
func windows(t *testing.T, size, free int64, bitlocker bool) Disk {
	tb, _ := gpt.New(size, gpt.GUIDFrom("win"))
	sec := func(b int64) uint64 { return uint64(b / sector) }
	esp, _ := tb.Add(gpt.Partition{Type: gpt.TypeESP, UUID: gpt.GUIDFrom("w-esp"), FirstLBA: 2048, LastLBA: 2048 + sec(100*MiB) - 1, Name: "EFI system partition"})
	msr, _ := tb.Add(gpt.Partition{Type: gpt.TypeMSReserved, UUID: gpt.GUIDFrom("w-msr"), FirstLBA: esp.LastLBA + 1, LastLBA: esp.LastLBA + sec(16*MiB), Name: "Microsoft reserved partition"})
	recFirst := tb.LastUsable() - sec(1*GiB) + 1
	cLast := recFirst - 1 - sec(free)
	c, _ := tb.Add(gpt.Partition{Type: gpt.TypeMSBasicData, UUID: gpt.GUIDFrom("w-c"), FirstLBA: msr.LastLBA + 1, LastLBA: cLast, Name: "Basic data partition"})
	rec, _ := tb.Add(gpt.Partition{Type: gpt.TypeWinRecovery, UUID: gpt.GUIDFrom("w-rec"), FirstLBA: recFirst, LastLBA: tb.LastUsable()})
	fs := "ntfs"
	if bitlocker {
		fs = "bitlocker"
	}
	return Disk{Path: "/dev/nvme0n1", Size: size, Table: tb, ESPFree: 60 * MiB, WindowsBoot: true,
		Parts: []PartInfo{{esp, "vfat"}, {msr, ""}, {c, fs}, {rec, "ntfs"}}}
}

func req(m Mode) Request {
	return Request{Mode: m, DiskGUID: gpt.GUIDFrom("new-disk"), ESPUUID: gpt.GUIDFrom("new-esp"), XbootUUID: gpt.GUIDFrom("new-xb")}
}

func TestEraseBlankDisk(t *testing.T) {
	src := medium(t)
	pl, err := MakePlan(src, blank(64*GiB), req(Erase))
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Copies) != 7 || !pl.Fresh || !pl.CopyESP || !pl.Enroll {
		t.Fatalf("%+v", pl)
	}
	for _, c := range pl.Copies {
		if c.Src.FirstLBA != c.Dst.FirstLBA || c.Src.Size() != c.Dst.Size() || c.Src.Type != c.Dst.Type || c.Src.Name != c.Dst.Name {
			t.Fatalf("layout changed: %+v → %+v", c.Src, c.Dst)
		}
		if c.Src.Type == gpt.TypeESP {
			if c.Dst.UUID == c.Src.UUID {
				t.Fatal("ESP must get a new UUID (first boot locates its disk by it)")
			}
		} else if c.Dst.UUID != c.Src.UUID {
			t.Fatal("usr partition UUIDs must be kept: usrhash= finds /usr by them")
		}
	}
	if pl.RootSpace < 50*GiB {
		t.Fatalf("root space %d", pl.RootSpace)
	}
}

func TestEraseRules(t *testing.T) {
	src := medium(t)
	if _, err := MakePlan(src, blank(14*GiB), req(Erase)); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("small disk: %v", err)
	}
	w := windows(t, 256*GiB, 100*GiB, false)
	if _, err := MakePlan(src, w, req(Erase)); !errors.Is(err, ErrNeedsConfirmation) || !strings.Contains(err.Error(), "Windows") {
		t.Fatalf("erase over Windows without confirmation: %v", err)
	}
	r := req(Erase)
	r.EraseConfirmed = true
	if _, err := MakePlan(src, w, r); err != nil {
		t.Fatal(err)
	}
	self := blank(64 * GiB)
	self.IsSource = true
	if _, err := MakePlan(src, self, req(Erase)); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("install onto medium: %v", err)
	}
}

func TestAlongsideWindows(t *testing.T) {
	src := medium(t)
	w := windows(t, 256*GiB, 100*GiB, false)
	before := append([]gpt.Partition(nil), w.Table.Partitions...)
	pl, err := MakePlan(src, w, req(Alongside))
	if err != nil {
		t.Fatal(err)
	}
	// Windows partitions are untouched, in the plan and in the probed table.
	for i, p := range before {
		if pl.Table.Partitions[i] != p || w.Table.Partitions[i] != p {
			t.Fatalf("Windows partition %d changed", i)
		}
	}
	if pl.Xbootldr == nil || pl.Xbootldr.Size() != XbootldrSize || pl.ESP.UUID != gpt.GUIDFrom("w-esp") {
		t.Fatal("XBOOTLDR / ESP reuse")
	}
	if len(pl.Copies) != 6 {
		t.Fatalf("copies %d (ESP is shared, not copied)", len(pl.Copies))
	}
	// Everything new lies inside the free gap between C: and recovery.
	gap := w.Table.FreeRegions()[0]
	for _, c := range pl.Copies {
		if c.Dst.FirstLBA < gap.First || c.Dst.LastLBA > gap.Last || c.Dst.FirstLBA%gpt.Align != 0 {
			t.Fatalf("partition outside gap or unaligned: %+v", c.Dst)
		}
	}
	if pl.Enroll || !strings.Contains(pl.Loader, "default auto-windows") || !strings.Contains(pl.Loader, "auto-entries yes") {
		t.Fatalf("loader: %q enroll=%v", pl.Loader, pl.Enroll)
	}
	r := req(Alongside)
	r.MakeDefault = true
	pl, _ = MakePlan(src, w, r)
	if !strings.Contains(pl.Loader, "default aurora_*") || !pl.BootFirst {
		t.Fatal(pl.Loader)
	}
}

func TestAlongsideRules(t *testing.T) {
	src := medium(t)
	if _, err := MakePlan(src, windows(t, 256*GiB, 30*GiB, false), req(Alongside)); !errors.Is(err, ErrNoSpace) || !strings.Contains(err.Error(), "never resizes") {
		t.Fatalf("too little free space: %v", err)
	}
	bl := windows(t, 256*GiB, 100*GiB, true)
	if _, err := MakePlan(src, bl, req(Alongside)); !errors.Is(err, ErrNeedsConfirmation) || !strings.Contains(err.Error(), "BitLocker") {
		t.Fatalf("BitLocker: %v", err)
	}
	r := req(Alongside)
	r.BitLockerConfirmed = true
	if _, err := MakePlan(src, bl, r); err != nil {
		t.Fatal(err)
	}
	small := windows(t, 256*GiB, 100*GiB, false)
	small.ESPFree = 10 * MiB
	if _, err := MakePlan(src, small, req(Alongside)); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("small ESP: %v", err)
	}
	if _, err := MakePlan(src, blank(64*GiB), req(Alongside)); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("alongside on blank disk: %v", err)
	}
}

// mediumA mimics an ISO: ESP and slot A only.
func mediumA(t *testing.T) Source {
	s := medium(t)
	tb, _ := gpt.New(8*GiB, gpt.GUIDFrom("iso"))
	for _, p := range s.Table.Partitions {
		if p.Name != "_empty" {
			p.Index = 0
			tb.Add(p)
		}
	}
	s.Table = tb
	return s
}

func TestMediumWithoutSlotB(t *testing.T) {
	src := mediumA(t)
	for _, tc := range []struct {
		d Disk
		m Mode
	}{{blank(64 * GiB), Erase}, {windows(t, 256*GiB, 100*GiB, false), Alongside}} {
		pl, err := MakePlan(src, tc.d, req(tc.m))
		if err != nil {
			t.Fatal(err)
		}
		if len(pl.Zero) != 3 {
			t.Fatalf("%s: expected 3 empty B partitions, got %d", tc.m, len(pl.Zero))
		}
		types := map[gpt.GUID]int{}
		for _, z := range pl.Zero {
			types[z.Type]++
			if z.Name != "_empty" || z.FirstLBA%gpt.Align != 0 {
				t.Fatalf("%+v", z)
			}
			for _, c := range pl.Copies {
				if c.Dst.FirstLBA <= z.LastLBA && z.FirstLBA <= c.Dst.LastLBA {
					t.Fatal("B slot overlaps a copied partition")
				}
			}
		}
		if types[gpt.TypeUsrX86_64] != 1 || types[gpt.TypeUsrVerityX86_64] != 1 || types[gpt.TypeUsrVeritySigX8664] != 1 {
			t.Fatal(types)
		}
	}
	// a full medium already carries B slots
	if pl, _ := MakePlan(medium(t), blank(64*GiB), req(Erase)); len(pl.Zero) != 0 {
		t.Fatal("unexpected extra slots")
	}
}
