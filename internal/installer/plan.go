// Package installer puts a Foundry OS from a live medium onto a target disk.
//
// The live system (installer UKI: root=tmpfs, first-boot partitioning
// masked) never writes its own medium. Installing copies the signed image
// partitions — ESP, usr/usr-verity/usr-verity-sig A and the empty B slots —
// onto the target; the installed system's first boot then creates the
// TPM-encrypted root exactly like a directly written image.
//
// Planning is pure (this file) so every layout decision is unit-tested;
// execution lives in apply.go.
package installer

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"foundry.dev/foundry/internal/gpt"
)

const (
	GiB              = int64(1) << 30
	MiB              = int64(1) << 20
	MinRoot          = 8 * GiB  // space left for the encrypted root
	AlongsideMinFree = 40 * GiB // unallocated space required beside Windows
	MinESPFree       = 20 * MiB // free space needed on a reused (Windows) ESP
	XbootldrSize     = 1 * GiB  // holds the UKIs when the ESP is shared
	sector           = int64(512)
)

type Mode string

const (
	Erase     Mode = "erase"
	Alongside Mode = "alongside"
)

// PartInfo is a partition on a disk plus what is on it.
type PartInfo struct {
	gpt.Partition
	FS string // "ntfs", "bitlocker", "vfat", "ext4", "" (unknown)
}

// Disk describes a candidate target disk.
type Disk struct {
	Path      string     `json:"path"`
	Size      int64      `json:"size"`
	Model     string     `json:"model,omitempty"`
	Serial    string     `json:"serial,omitempty"`
	Transport string     `json:"transport,omitempty"`
	Removable bool       `json:"removable"`
	Table     *gpt.Table `json:"-"`
	Foreign   bool       `json:"foreign,omitempty"` // MBR or unreadable table: erase only
	Parts     []PartInfo `json:"-"`
	// Filled by probing the ESP (alongside mode).
	ESPFree     int64 `json:"espFree,omitempty"`
	WindowsBoot bool  `json:"windows,omitempty"`
	BitLocker   bool  `json:"bitlocker,omitempty"`
	IsSource    bool  `json:"isSource,omitempty"`
}

// Source is the live medium we booted from.
type Source struct {
	Path  string
	Size  int64
	Table *gpt.Table
	OSID  string // branding id, e.g. "aurora" (UKI file prefix)
	Arch  string // "x86_64" | "aarch64"
}

// Request is what the user (or an unattended config) asked for.
type Request struct {
	Target             string   `json:"target"`
	Mode               Mode     `json:"mode"`
	EraseConfirmed     bool     `json:"eraseConfirmed"`     // target has data that will be destroyed
	BitLockerConfirmed bool     `json:"bitlockerConfirmed"` // user holds the BitLocker recovery key
	MakeDefault        bool     `json:"makeDefault"`        // boot this OS by default in dual boot
	DiskGUID           gpt.GUID `json:"-"`                  // new disk GUID (erase); random in production
	ESPUUID            gpt.GUID `json:"-"`                  // new ESP UUID (erase); must differ from the medium
	XbootUUID          gpt.GUID `json:"-"`
}

// Copy moves one partition's bytes from the source to the target.
type Copy struct {
	Src gpt.Partition
	Dst gpt.Partition
}

// Plan is everything apply needs; nothing in it is decided at apply time.
type Plan struct {
	Mode       Mode
	Target     string
	TargetSize int64
	Table      *gpt.Table // final table of the target
	Fresh      bool       // Table is new (erase) — write protective MBR too
	Copies     []Copy
	Zero       []gpt.Partition // created empty (update slots missing on the medium); zero-filled when small
	ESP        gpt.Partition
	Xbootldr   *gpt.Partition // created and formatted (alongside)
	CopyESP    bool           // erase: ESP copied block-for-block
	Loader     string         // loader.conf to write (alongside); "" keeps the copied one
	Enroll     bool           // Secure Boot key enrollment files stay on the ESP
	BootFirst  bool           // put our NVRAM entry first
	RootSpace  int64          // space the first boot will use for root
	Notes      []string
}

var (
	ErrNeedsConfirmation = errors.New("installer: confirmation required")
	ErrNoSpace           = errors.New("installer: not enough space")
	ErrUnsupported       = errors.New("installer: unsupported disk layout")
)

func isRoot(t gpt.GUID) bool {
	return t == gpt.TypeRootX86_64 || t == gpt.TypeRootArm64 || t == gpt.TypeLinuxGeneric
}

// imageParts returns the partitions of the medium that make up the OS image
// (everything but a root partition, which only exists if the medium itself
// was booted normally), ordered by position.
func imageParts(src Source) ([]gpt.Partition, error) {
	var ps []gpt.Partition
	esp := 0
	for _, p := range src.Table.Partitions {
		if isRoot(p.Type) {
			continue
		}
		if p.Type == gpt.TypeESP {
			esp++
		}
		ps = append(ps, p)
	}
	if esp != 1 {
		return nil, fmt.Errorf("%w: the install medium has %d ESPs", ErrUnsupported, esp)
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].FirstLBA < ps[j].FirstLBA })
	return ps, nil
}

// slotB returns the empty update-slot partitions to create when the medium
// carries only slot A (ISO media): same types and sizes as slot A.
func slotB(parts []gpt.Partition, seed gpt.GUID) []gpt.Partition {
	n := 0
	for _, p := range parts {
		if p.Name == "_empty" {
			n++
		}
	}
	if n > 0 {
		return nil
	}
	var out []gpt.Partition
	for _, p := range parts {
		if p.Type == gpt.TypeESP {
			continue
		}
		out = append(out, gpt.Partition{Type: p.Type, UUID: gpt.GUIDFrom("foundry-slot-b", seed.String(), p.UUID.String()),
			FirstLBA: 0, LastLBA: p.LastLBA - p.FirstLBA, Attrs: p.Attrs, Name: "_empty"})
	}
	return out
}

// MakePlan decides the target layout.
func MakePlan(src Source, d Disk, req Request) (*Plan, error) {
	if d.IsSource || d.Path == src.Path {
		return nil, fmt.Errorf("%w: cannot install onto the medium the installer runs from", ErrUnsupported)
	}
	parts, err := imageParts(src)
	if err != nil {
		return nil, err
	}
	switch req.Mode {
	case Erase:
		return planErase(src, d, req, parts)
	case Alongside:
		return planAlongside(src, d, req, parts)
	}
	return nil, fmt.Errorf("installer: unknown mode %q", req.Mode)
}

func planErase(src Source, d Disk, req Request, parts []gpt.Partition) (*Plan, error) {
	if (len(d.Parts) > 0 || d.Foreign) && !req.EraseConfirmed {
		what := "all data on " + d.Path
		if d.WindowsBoot || hasFS(d, "ntfs") || d.BitLocker {
			what = "Windows and all data on " + d.Path
		}
		return nil, fmt.Errorf("%w: %s will be erased", ErrNeedsConfirmation, what)
	}
	end := int64(0)
	for _, p := range parts {
		if e := int64(p.LastLBA+1) * sector; e > end {
			end = e
		}
	}
	if need := end + MinRoot + 34*sector; d.Size < need {
		return nil, fmt.Errorf("%w: %s has %s, this OS needs at least %s", ErrNoSpace, d.Path, human(d.Size), human(need))
	}
	if req.ESPUUID.IsZero() {
		return nil, errors.New("installer: a new ESP UUID is required")
	}
	t, err := gpt.New(d.Size, req.DiskGUID)
	if err != nil {
		return nil, err
	}
	pl := &Plan{Mode: Erase, Target: d.Path, TargetSize: d.Size, Table: t, Fresh: true, CopyESP: true, Enroll: true, BootFirst: true}
	for _, p := range parts {
		dst := gpt.Partition{Type: p.Type, UUID: p.UUID, FirstLBA: p.FirstLBA, LastLBA: p.LastLBA, Attrs: p.Attrs, Name: p.Name}
		if p.Type == gpt.TypeESP {
			// A fresh UUID: the medium may stay plugged in, and first boot
			// finds its disk through the ESP's partition UUID.
			dst.UUID = req.ESPUUID
		}
		added, err := t.Add(dst)
		if err != nil {
			return nil, err
		}
		if p.Type == gpt.TypeESP {
			pl.ESP = added
		}
		pl.Copies = append(pl.Copies, Copy{Src: p, Dst: added})
	}
	next := uint64((end/sector + gpt.Align - 1) / gpt.Align * gpt.Align)
	for _, b := range slotB(parts, req.DiskGUID) {
		size := b.LastLBA + 1
		b.FirstLBA, b.LastLBA = next, next+size-1
		added, err := t.Add(b)
		if err != nil {
			return nil, err
		}
		pl.Zero = append(pl.Zero, added)
		next = (added.LastLBA + 1 + gpt.Align - 1) / gpt.Align * gpt.Align
		end = int64(added.LastLBA+1) * sector
	}
	if need := end + MinRoot + 34*sector; d.Size < need {
		return nil, fmt.Errorf("%w: %s has %s, this OS needs at least %s", ErrNoSpace, d.Path, human(d.Size), human(need))
	}
	pl.RootSpace = d.Size - end - 34*sector
	pl.Notes = append(pl.Notes, fmt.Sprintf("%s will be erased; %s go to the encrypted system and data partition.", d.Path, human(pl.RootSpace)))
	return pl, nil
}

func hasFS(d Disk, fs string) bool {
	for _, p := range d.Parts {
		if p.FS == fs {
			return true
		}
	}
	return false
}

func planAlongside(src Source, d Disk, req Request, parts []gpt.Partition) (*Plan, error) {
	if d.Table == nil || d.Foreign {
		return nil, fmt.Errorf("%w: installing alongside needs a GPT disk (Windows in UEFI mode)", ErrUnsupported)
	}
	var esps []PartInfo
	for _, p := range d.Parts {
		if p.Type == gpt.TypeESP {
			esps = append(esps, p)
		}
	}
	if len(esps) != 1 {
		return nil, fmt.Errorf("%w: expected one EFI system partition on %s, found %d", ErrUnsupported, d.Path, len(esps))
	}
	if d.ESPFree < MinESPFree {
		return nil, fmt.Errorf("%w: the EFI system partition has %s free; %s are needed. Free space on it in Windows first.", ErrNoSpace, human(d.ESPFree), human(MinESPFree))
	}
	if (d.BitLocker || hasFS(d, "bitlocker")) && !req.BitLockerConfirmed {
		return nil, fmt.Errorf("%w: BitLocker is on. Changing the boot menu can make Windows ask for the BitLocker recovery key; confirm that you have it (or suspend BitLocker in Windows first)", ErrNeedsConfirmation)
	}
	var osBytes int64
	for _, p := range parts {
		if p.Type != gpt.TypeESP {
			osBytes += p.Size() + gpt.Align*sector // alignment slack per partition
		}
	}
	bs := slotB(parts, req.XbootUUID)
	for _, b := range bs {
		osBytes += int64(b.LastLBA+1)*sector + gpt.Align*sector
	}
	need := XbootldrSize + osBytes + MinRoot
	if need < AlongsideMinFree {
		need = AlongsideMinFree
	}
	free := d.Table.FreeRegions()
	if len(free) == 0 || free[0].Bytes() < need {
		have := int64(0)
		if len(free) > 0 {
			have = free[0].Bytes()
		}
		return nil, fmt.Errorf("%w: the largest unallocated space on %s is %s; %s are needed. Shrink the Windows partition in Disk Management first (Foundry never resizes Windows).", ErrNoSpace, d.Path, human(have), human(need))
	}
	reg := free[0]
	// Work on a copy of the table: the plan must not mutate the probe result.
	t, err := cloneTable(d.Table)
	if err != nil {
		return nil, err
	}
	pl := &Plan{Mode: Alongside, Target: d.Path, TargetSize: d.Size, Table: t, ESP: esps[0].Partition, BootFirst: req.MakeDefault}
	next := reg.First
	place := func(p gpt.Partition, sectors uint64) (gpt.Partition, error) {
		p.FirstLBA = next
		p.LastLBA = next + sectors - 1
		next = (p.LastLBA + 1 + gpt.Align - 1) / gpt.Align * gpt.Align
		return t.Add(p)
	}
	if req.XbootUUID.IsZero() {
		return nil, errors.New("installer: a new XBOOTLDR UUID is required")
	}
	xb, err := place(gpt.Partition{Type: gpt.TypeXBOOTLDR, UUID: req.XbootUUID, Name: "XBOOTLDR"}, uint64(XbootldrSize/sector))
	if err != nil {
		return nil, err
	}
	pl.Xbootldr = &xb
	for _, p := range parts {
		if p.Type == gpt.TypeESP {
			continue
		}
		dst, err := place(gpt.Partition{Type: p.Type, UUID: p.UUID, Attrs: p.Attrs, Name: p.Name}, p.LastLBA-p.FirstLBA+1)
		if err != nil {
			return nil, err
		}
		pl.Copies = append(pl.Copies, Copy{Src: p, Dst: dst})
	}
	for _, b := range bs {
		dst, err := place(gpt.Partition{Type: b.Type, UUID: b.UUID, Attrs: b.Attrs, Name: b.Name}, b.LastLBA+1)
		if err != nil {
			return nil, err
		}
		pl.Zero = append(pl.Zero, dst)
	}
	pl.RootSpace = int64(reg.Last-next+1) * sector
	def := "auto-windows"
	if req.MakeDefault {
		def = src.OSID + "_*"
	}
	pl.Loader = "timeout 3\nconsole-mode keep\neditor no\nauto-entries yes\nauto-firmware yes\ndefault " + def + "\n"
	pl.Notes = append(pl.Notes,
		fmt.Sprintf("Windows stays untouched; this OS uses %s of the %s unallocated space.", human(int64(next-reg.First)*sector+pl.RootSpace), human(reg.Bytes())),
		"The boot menu shows both systems for 3 seconds.",
		"Secure Boot keys are not changed in dual-boot mode.")
	return pl, nil
}

func cloneTable(t *gpt.Table) (*gpt.Table, error) {
	return t.Clone(), nil
}

func human(b int64) string {
	switch {
	case b >= GiB:
		return fmt.Sprintf("%.1f GiB", float64(b)/float64(GiB))
	case b >= MiB:
		return fmt.Sprintf("%d MiB", b/MiB)
	}
	return fmt.Sprintf("%d bytes", b)
}

// Summary is a human-readable description of a plan.
func (p *Plan) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Mode: %s on %s\n", p.Mode, p.Target)
	for _, c := range p.Copies {
		fmt.Fprintf(&b, "  copy %-36q %8s → sectors %d–%d\n", c.Src.Name, human(c.Src.Size()), c.Dst.FirstLBA, c.Dst.LastLBA)
	}
	if p.Xbootldr != nil {
		fmt.Fprintf(&b, "  new boot partition (XBOOTLDR) %s\n", human(p.Xbootldr.Size()))
	}
	for _, n := range p.Notes {
		fmt.Fprintf(&b, "  • %s\n", n)
	}
	return b.String()
}
