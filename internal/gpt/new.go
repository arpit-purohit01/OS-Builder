package gpt

import (
	"encoding/binary"
	"errors"
	"io"
	"sort"
	"unicode/utf16"
)

// More partition types used by the installer.
var (
	TypeXBOOTLDR     = MustParse("bc13c2ff-59e6-4262-a352-b275fd6f7172")
	TypeMSReserved   = MustParse("e3c9e316-0b5c-4db8-817d-f92df00215ae")
	TypeMSBasicData  = MustParse("ebd0a0a2-b9e5-4433-87c0-68b6b72699c7")
	TypeWinRecovery  = MustParse("de94bba4-06d1-4d40-a16a-bfd50179d6ac")
	TypeRootX86_64   = MustParse("4f68bce3-e8cd-4db1-96e7-fbcaf984b709")
	TypeRootArm64    = MustParse("b921b045-1df0-41c3-af44-4c6f280d3fae")
	TypeLinuxGeneric = MustParse("0fc63daf-8483-4772-8e79-3d69d8477de4")
)

// Align is the partition alignment used for new partitions (1 MiB).
const Align = 2048

// New returns an empty GPT for a disk of size bytes (512-byte sectors,
// 128 entries), ready for Add and Write.
func New(size int64, diskGUID GUID) (*Table, error) {
	sectors := uint64(size / sector)
	if sectors < 2*Align+68 {
		return nil, errors.New("gpt: disk too small")
	}
	h := header{Revision: 0x00010000, HeaderSize: 92, CurrentLBA: 1, BackupLBA: sectors - 1,
		FirstUsable: 34, LastUsable: sectors - 34, DiskGUID: diskGUID, EntriesLBA: 2, NumEntries: 128, EntrySize: 128}
	copy(h.Signature[:], "EFI PART")
	return &Table{hdr: h, entries: make([]byte, 128*128)}, nil
}

// Retarget moves the backup header to the end of a (larger) disk, e.g. after
// copying an image's table to a bigger device.
func (t *Table) Retarget(size int64) error {
	sectors := uint64(size / sector)
	for _, p := range t.Partitions {
		if p.LastLBA > sectors-34 {
			return errors.New("gpt: disk smaller than the partitions")
		}
	}
	t.hdr.BackupLBA, t.hdr.LastUsable = sectors-1, sectors-34
	return nil
}

// DiskGUID returns the disk identifier.
func (t *Table) DiskGUID() GUID { return t.hdr.DiskGUID }

// WriteProtectiveMBR writes the protective MBR that marks the disk as GPT.
func WriteProtectiveMBR(w io.WriterAt, size int64) error {
	mbr := make([]byte, sector)
	e := mbr[446:]
	e[1], e[2], e[3] = 0x00, 0x02, 0x00 // CHS start
	e[4] = 0xEE
	e[5], e[6], e[7] = 0xFF, 0xFF, 0xFF
	binary.LittleEndian.PutUint32(e[8:], 1)
	n := uint64(size/sector) - 1
	if n > 0xFFFFFFFF {
		n = 0xFFFFFFFF
	}
	binary.LittleEndian.PutUint32(e[12:], uint32(n))
	mbr[510], mbr[511] = 0x55, 0xAA
	_, err := w.WriteAt(mbr, 0)
	return err
}

// Region is a run of unpartitioned sectors.
type Region struct{ First, Last uint64 }

// Bytes is the region size.
func (r Region) Bytes() int64 { return int64(r.Last-r.First+1) * sector }

// FreeRegions lists aligned unpartitioned runs, largest first.
func (t *Table) FreeRegions() []Region {
	ps := append([]Partition(nil), t.Partitions...)
	sort.Slice(ps, func(i, j int) bool { return ps[i].FirstLBA < ps[j].FirstLBA })
	var out []Region
	add := func(first, last uint64) {
		first = (first + Align - 1) / Align * Align
		if last >= first && last-first+1 >= Align {
			out = append(out, Region{first, last})
		}
	}
	next := t.hdr.FirstUsable
	for _, p := range ps {
		if p.FirstLBA > next {
			add(next, p.FirstLBA-1)
		}
		if p.LastLBA+1 > next {
			next = p.LastLBA + 1
		}
	}
	if t.hdr.LastUsable >= next {
		add(next, t.hdr.LastUsable)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Bytes() > out[j].Bytes() })
	return out
}

// Clone returns a deep copy (planning works on copies).
func (t *Table) Clone() *Table {
	c := &Table{hdr: t.hdr, entries: append([]byte(nil), t.entries...)}
	c.Partitions = append([]Partition(nil), t.Partitions...)
	return c
}

// Set rewrites the entry of an existing partition (same index): used to give
// partitions created by other tools (xorriso) their required UUIDs and names.
func (t *Table) Set(p Partition) error {
	if p.Index < 1 || p.Index > int(t.hdr.NumEntries) {
		return errors.New("gpt: bad partition index")
	}
	if len(p.Name) > 36 {
		return errors.New("gpt: name longer than 36 characters")
	}
	e := t.entries[(p.Index-1)*int(t.hdr.EntrySize) : p.Index*int(t.hdr.EntrySize)]
	clear(e)
	copy(e[0:16], p.Type[:])
	copy(e[16:32], p.UUID[:])
	binary.LittleEndian.PutUint64(e[32:], p.FirstLBA)
	binary.LittleEndian.PutUint64(e[40:], p.LastLBA)
	binary.LittleEndian.PutUint64(e[48:], p.Attrs)
	for j, c := range utf16.Encode([]rune(p.Name)) {
		binary.LittleEndian.PutUint16(e[56+2*j:], c)
	}
	for i := range t.Partitions {
		if t.Partitions[i].Index == p.Index {
			t.Partitions[i] = p
			return nil
		}
	}
	t.Partitions = append(t.Partitions, p)
	return nil
}
