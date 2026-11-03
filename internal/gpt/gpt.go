// Package gpt reads and edits GUID partition tables in disk image files.
// It is used by the finalize stage to add the deferred usr-verity-sig
// partition entry without touching any partition data that is covered by
// hashes (usr, usr-verity).
package gpt

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"strings"
	"unicode/utf16"
)

const sector = 512

// Well-known partition type GUIDs (Discoverable Partitions Specification).
var (
	TypeESP               = MustParse("c12a7328-f81f-11d2-ba4b-00a0c93ec93b")
	TypeUsrX86_64         = MustParse("8484680c-9521-48c6-9c11-b0720656f69e")
	TypeUsrVerityX86_64   = MustParse("77ff5f63-e7b6-4633-acf4-1565b864c0e6")
	TypeUsrVeritySigX8664 = MustParse("e7bb33fb-06cf-4e81-8273-e543b413e2e2")
	TypeUsrArm64          = MustParse("b0e01050-ee5f-4390-949a-9101b17104e9")
	TypeUsrVerityArm64    = MustParse("6e11a4e7-fbca-4ded-b9e9-e1a512bb664e")
	TypeUsrVeritySigArm64 = MustParse("c23ce4ff-44bd-4b00-b2d4-b41b3419e02a")
)

// GUID is a mixed-endian on-disk GUID.
type GUID [16]byte

// MustParse parses a textual GUID or panics.
func MustParse(s string) GUID {
	g, err := ParseGUID(s)
	if err != nil {
		panic(err)
	}
	return g
}

// ParseGUID parses "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx" into on-disk layout.
func ParseGUID(s string) (GUID, error) {
	var g GUID
	h := strings.ReplaceAll(s, "-", "")
	if len(h) != 32 || len(s) != 36 {
		return g, fmt.Errorf("gpt: bad guid %q", s)
	}
	var b [16]byte
	for i := 0; i < 16; i++ {
		var v byte
		if _, err := fmt.Sscanf(h[2*i:2*i+2], "%02x", &v); err != nil {
			return g, fmt.Errorf("gpt: bad guid %q", s)
		}
		b[i] = v
	}
	// first three fields are little endian on disk
	g[0], g[1], g[2], g[3] = b[3], b[2], b[1], b[0]
	g[4], g[5] = b[5], b[4]
	g[6], g[7] = b[7], b[6]
	copy(g[8:], b[8:])
	return g, nil
}

func (g GUID) String() string {
	return fmt.Sprintf("%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		g[3], g[2], g[1], g[0], g[5], g[4], g[7], g[6], g[8], g[9], g[10], g[11], g[12], g[13], g[14], g[15])
}

// IsZero reports an unused entry.
func (g GUID) IsZero() bool { return g == GUID{} }

// GUIDFrom derives a deterministic RFC 4122 v4-shaped GUID from data.
func GUIDFrom(data ...string) GUID {
	h := sha256.Sum256([]byte(strings.Join(data, "\x00")))
	var g GUID
	copy(g[:], h[:16])
	g[7] = (g[7] & 0x0f) | 0x40 // version 4 (byte 7 holds the high byte of field 3 on disk)
	g[8] = (g[8] & 0x3f) | 0x80
	return g
}

// Partition is one used GPT entry.
type Partition struct {
	Index    int // 1-based entry number
	Type     GUID
	UUID     GUID
	FirstLBA uint64
	LastLBA  uint64
	Attrs    uint64
	Name     string
}

// Offset returns the byte offset of the partition.
func (p Partition) Offset() int64 { return int64(p.FirstLBA) * sector }

// Size returns the partition size in bytes.
func (p Partition) Size() int64 { return int64(p.LastLBA-p.FirstLBA+1) * sector }

type header struct {
	Signature   [8]byte
	Revision    uint32
	HeaderSize  uint32
	HeaderCRC   uint32
	Reserved    uint32
	CurrentLBA  uint64
	BackupLBA   uint64
	FirstUsable uint64
	LastUsable  uint64
	DiskGUID    GUID
	EntriesLBA  uint64
	NumEntries  uint32
	EntrySize   uint32
	EntriesCRC  uint32
}

// Table is a parsed GPT.
type Table struct {
	hdr        header
	entries    []byte // raw entry array
	Partitions []Partition
}

// FirstUsable / LastUsable LBAs.
func (t *Table) FirstUsable() uint64 { return t.hdr.FirstUsable }
func (t *Table) LastUsable() uint64  { return t.hdr.LastUsable }

// Read parses the primary GPT of an image and validates its CRCs.
func Read(r io.ReaderAt) (*Table, error) {
	buf := make([]byte, sector)
	if _, err := r.ReadAt(buf, sector); err != nil {
		return nil, err
	}
	var h header
	binary.Read(bytes.NewReader(buf), binary.LittleEndian, &h)
	if string(h.Signature[:]) != "EFI PART" {
		return nil, errors.New("gpt: no GPT header")
	}
	if h.HeaderSize < 92 || h.HeaderSize > sector || h.EntrySize < 128 || h.NumEntries > 1024 {
		return nil, errors.New("gpt: unsupported header")
	}
	hb := append([]byte{}, buf[:h.HeaderSize]...)
	binary.LittleEndian.PutUint32(hb[16:], 0)
	if crc32.ChecksumIEEE(hb) != h.HeaderCRC {
		return nil, errors.New("gpt: header CRC mismatch")
	}
	ents := make([]byte, int(h.NumEntries)*int(h.EntrySize))
	if _, err := r.ReadAt(ents, int64(h.EntriesLBA)*sector); err != nil {
		return nil, err
	}
	if crc32.ChecksumIEEE(ents) != h.EntriesCRC {
		return nil, errors.New("gpt: entries CRC mismatch")
	}
	t := &Table{hdr: h, entries: ents}
	for i := 0; i < int(h.NumEntries); i++ {
		e := ents[i*int(h.EntrySize):]
		var p Partition
		copy(p.Type[:], e[0:16])
		if p.Type.IsZero() {
			continue
		}
		copy(p.UUID[:], e[16:32])
		p.FirstLBA = binary.LittleEndian.Uint64(e[32:])
		p.LastLBA = binary.LittleEndian.Uint64(e[40:])
		p.Attrs = binary.LittleEndian.Uint64(e[48:])
		u := make([]uint16, 36)
		for j := range u {
			u[j] = binary.LittleEndian.Uint16(e[56+2*j:])
		}
		n := 0
		for n < len(u) && u[n] != 0 {
			n++
		}
		p.Name = string(utf16.Decode(u[:n]))
		p.Index = i + 1
		t.Partitions = append(t.Partitions, p)
	}
	return t, nil
}

// ReadFile opens and parses an image file.
func ReadFile(path string) (*Table, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Read(f)
}

// ByName returns the partition with a given label.
func (t *Table) ByName(name string) (Partition, bool) {
	for _, p := range t.Partitions {
		if p.Name == name {
			return p, true
		}
	}
	return Partition{}, false
}

// ByType returns partitions of a type, in disk order.
func (t *Table) ByType(g GUID) []Partition {
	var out []Partition
	for _, p := range t.Partitions {
		if p.Type == g {
			out = append(out, p)
		}
	}
	return out
}

// Free reports whether [first,last] overlaps no partition and is usable.
func (t *Table) Free(first, last uint64) bool {
	if first < t.hdr.FirstUsable || last > t.hdr.LastUsable || last < first {
		return false
	}
	for _, p := range t.Partitions {
		if first <= p.LastLBA && p.FirstLBA <= last {
			return false
		}
	}
	return true
}

// Add inserts a partition into the first unused entry slot.
func (t *Table) Add(p Partition) (Partition, error) {
	if !t.Free(p.FirstLBA, p.LastLBA) {
		return p, fmt.Errorf("gpt: range %d-%d is not free", p.FirstLBA, p.LastLBA)
	}
	if len(p.Name) > 36 {
		return p, errors.New("gpt: name longer than 36 characters")
	}
	for i := 0; i < int(t.hdr.NumEntries); i++ {
		e := t.entries[i*int(t.hdr.EntrySize) : (i+1)*int(t.hdr.EntrySize)]
		if !GUID(e[0:16]).IsZero() {
			continue
		}
		clear(e)
		copy(e[0:16], p.Type[:])
		copy(e[16:32], p.UUID[:])
		binary.LittleEndian.PutUint64(e[32:], p.FirstLBA)
		binary.LittleEndian.PutUint64(e[40:], p.LastLBA)
		binary.LittleEndian.PutUint64(e[48:], p.Attrs)
		for j, c := range utf16.Encode([]rune(p.Name)) {
			binary.LittleEndian.PutUint16(e[56+2*j:], c)
		}
		p.Index = i + 1
		t.Partitions = append(t.Partitions, p)
		return p, nil
	}
	return p, errors.New("gpt: no free entry")
}

// Write stores the primary and backup tables with fresh CRCs.
func (t *Table) Write(w io.WriterAt) error {
	crc := crc32.ChecksumIEEE(t.entries)
	write := func(h header) error {
		h.EntriesCRC = crc
		h.HeaderCRC = 0
		var b bytes.Buffer
		binary.Write(&b, binary.LittleEndian, h)
		hb := b.Bytes()[:h.HeaderSize]
		h.HeaderCRC = crc32.ChecksumIEEE(hb)
		b.Reset()
		binary.Write(&b, binary.LittleEndian, h)
		sec := make([]byte, sector)
		copy(sec, b.Bytes()[:h.HeaderSize])
		if _, err := w.WriteAt(t.entries, int64(h.EntriesLBA)*sector); err != nil {
			return err
		}
		_, err := w.WriteAt(sec, int64(h.CurrentLBA)*sector)
		return err
	}
	if err := write(t.hdr); err != nil {
		return err
	}
	b := t.hdr
	b.CurrentLBA, b.BackupLBA = t.hdr.BackupLBA, t.hdr.CurrentLBA
	entrySectors := (uint64(len(t.entries)) + sector - 1) / sector
	b.EntriesLBA = t.hdr.BackupLBA - entrySectors
	return write(b)
}
