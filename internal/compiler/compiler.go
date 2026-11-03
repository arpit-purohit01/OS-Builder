// Package compiler turns a validated OSSpec into a mkosi project tree.
//
// Compile is a pure function: no network, filesystem, clock or randomness.
// Identical inputs give a byte-identical Tree (golden-tested). See
// docs/context/build-pipeline.md and docs/context/boot-firmware-and-disk.md.
package compiler

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"foundry.dev/foundry/internal/catalog"
	"foundry.dev/foundry/internal/osspec"
)

//go:embed files
var files embed.FS

func embedded(p string) string {
	b, err := files.ReadFile("files/" + p)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// Platform carries deployment settings that are not part of the spec but
// still shape the image (they are part of the build cache key).
type Platform struct {
	Tenant        string `json:"tenant"`
	FleetURL      string `json:"fleetUrl"`
	UpdateURL     string `json:"updateUrl"`
	SyncURL       string `json:"syncUrl,omitempty"`
	FlatpakRemote string `json:"flatpakRemote,omitempty"` // contents of a .flatpakrepo file
}

// Input to Compile.
type Input struct {
	Spec       *osspec.Spec
	SpecDigest string
	Resolution *catalog.Resolution
	Catalog    string // catalog version
	Platform   Platform
	AssetExt   map[string]string // asset digest -> file extension (".png")
	// DebPackages maps uploaded .deb digests to their package names (read
	// from the control file at upload).
	DebPackages map[string]string
	// Layouts maps uploaded layout digests to their validated documents
	// (look.layout.custom).
	Layouts map[string]*osspec.LayoutDoc
}

// Output of Compile.
type Output struct {
	Tree        *Tree
	ImageName   string // mkosi output stem: <osId>_<version>
	Packages    []string
	Labels      Labels
	Cmdline     []string
	SourceEpoch int64
	Warnings    []string
}

// Labels are GPT partition labels shared by build, sysupdate and agent.
type Labels struct {
	Prefix string // osId (≤ 20 chars)
	USR    string // prefix_version
	Verity string
	Sig    string
}

// SlotSizes in bytes.
type SlotSizes struct{ USR, Verity int64 }

const (
	gib = int64(1) << 30
	mib = int64(1) << 20
)

// Slots returns A/B slot sizes for a profile.
func Slots(profile string) SlotSizes {
	switch profile {
	case "desktop-gnome", "desktop-kde":
		return SlotSizes{USR: 8 * gib, Verity: 128 * mib}
	default:
		return SlotSizes{USR: 3 * gib, Verity: 64 * mib}
	}
}

// MakeLabels derives partition labels (GPT names are limited to 36 UTF-16 units).
func MakeLabels(osID, version string) Labels {
	p := osID
	if len(p) > 20 {
		p = p[:20]
	}
	v := version
	if len(v) > 13 {
		v = v[:13]
	}
	u := p + "_" + v
	return Labels{Prefix: p, USR: u, Verity: u + "_v", Sig: u + "_s"}
}

type ctx struct {
	in         Input
	s          *osspec.Spec
	base       Base
	t          *Tree
	pkgs       map[string]bool
	cmdline    []string
	presets    []string // enable/disable lines
	post       []string // postinst script fragments (fixed text, no user input)
	postLate   []string // fragments after the platform's own (customer scripts)
	extraTrees []string
	values     map[string]any
	warn       []string
	labels     Labels
}

// Compile builds the mkosi project for a validated spec.
func Compile(in Input) (*Output, error) {
	if in.Spec == nil || in.Resolution == nil || !in.Resolution.OK {
		return nil, fmt.Errorf("compiler: spec must be validated (catalog resolution OK) before compiling")
	}
	b, err := LookupBase(in.Spec.Foundation.Base)
	if err != nil {
		return nil, err
	}
	s := in.Spec
	c := &ctx{in: in, s: s, base: b, t: newTree(), pkgs: map[string]bool{}, values: map[string]any{}}
	c.labels = MakeLabels(s.Branding.OSID, s.Metadata.Version)
	for _, p := range b.Packages {
		c.pkgs[p] = true
	}
	if c.graphical() {
		for _, p := range b.GraphicalPkg {
			c.pkgs[p] = true
		}
	}
	for _, p := range in.Resolution.Packages {
		c.pkgs[p] = true
	}
	epoch, err := c.sourceEpoch()
	if err != nil {
		return nil, err
	}

	c.partitions()
	c.initrd()
	c.network()
	c.branding()
	c.security()
	c.access()
	c.kernel()
	c.memory()
	c.look()
	c.apps()
	c.ecosystem()
	c.updates()
	c.health()
	c.installer()
	c.expert()
	c.mkosiConf(epoch)
	c.postinst()
	c.presetFile()
	c.valuesFile()

	pk := make([]string, 0, len(c.pkgs))
	for p := range c.pkgs {
		pk = append(pk, p)
	}
	sort.Strings(pk)
	return &Output{Tree: c.t, ImageName: s.Branding.OSID + "_" + s.Metadata.Version, Packages: pk, Labels: c.labels, Cmdline: c.cmdline, SourceEpoch: epoch, Warnings: c.warn}, nil
}

func (c *ctx) graphical() bool {
	switch c.s.Foundation.Profile {
	case "desktop-gnome", "desktop-kde", "kiosk-cage", "thin-client":
		return true
	}
	return false
}

func (c *ctx) desktop() bool {
	return c.s.Foundation.Profile == "desktop-gnome" || c.s.Foundation.Profile == "desktop-kde"
}

func (c *ctx) kioskLike() bool {
	return c.s.Foundation.Profile == "kiosk-cage" || c.s.Foundation.Profile == "thin-client"
}

func (c *ctx) sourceEpoch() (int64, error) {
	if c.s.Foundation.Snapshot == "" {
		// Unpinned builds still get a fixed epoch derived from the spec digest
		// so file timestamps are deterministic; package versions are not.
		c.warn = append(c.warn, "No snapshot pinned: package versions follow the live mirror, so builds are not reproducible across days.")
		return 1767225600, nil // 2026-01-01T00:00:00Z
	}
	_, e, err := snapshotStamp(c.s.Foundation.Snapshot)
	return e, err
}

// seed is a deterministic UUID derived from the spec digest (partition UUIDs).
func (c *ctx) seed() string {
	h := sha256.Sum256([]byte("foundry-seed-v1|" + c.in.SpecDigest))
	x := hex.EncodeToString(h[:16])
	return fmt.Sprintf("%s-%s-4%s-8%s-%s", x[0:8], x[8:12], x[13:16], x[17:20], x[20:32])
}

func (c *ctx) mkosiConf(epoch int64) {
	b, s := c.base, c.s
	var w strings.Builder
	w.WriteString("# Generated by Foundry compiler. Do not edit.\n")
	fmt.Fprintf(&w, "# spec %s catalog %s\n\n", c.in.SpecDigest, c.in.Catalog)
	w.WriteString("[Distribution]\n")
	fmt.Fprintf(&w, "Distribution=%s\nRelease=%s\nRepositories=%s\n", b.Distribution, b.Release, strings.Join(b.Repositories, ","))
	if s.Foundation.Snapshot != "" && b.SnapshotURL != "" {
		st, _, _ := snapshotStamp(s.Foundation.Snapshot)
		fmt.Fprintf(&w, "Mirror=%s\n", fmt.Sprintf(b.SnapshotURL, st))
	} else {
		fmt.Fprintf(&w, "Mirror=%s\n", b.Mirror)
	}
	arch := map[string]string{"x86_64": "x86-64", "aarch64": "arm64"}[s.Foundation.Arch[0]]
	fmt.Fprintf(&w, "Architecture=%s\n\n", arch)

	w.WriteString("[Output]\nFormat=disk\n")
	fmt.Fprintf(&w, "ImageId=%s\nImageVersion=%s\n", s.Branding.OSID, s.Metadata.Version)
	fmt.Fprintf(&w, "Seed=%s\n", c.seed())
	w.WriteString("ManifestFormat=json\nSplitArtifacts=uki,partitions,roothash,kernel,initrd,os-release\n\n")

	w.WriteString("[Content]\nBootable=yes\nBootloader=systemd-boot\nUnifiedKernelImages=unsigned\n")
	w.WriteString("CleanPackageMetadata=yes\nWithDocs=no\n")
	fmt.Fprintf(&w, "UnifiedKernelImageFormat=%s\n", c.labels.USR)
	fmt.Fprintf(&w, "SourceDateEpoch=%d\n", epoch)
	fmt.Fprintf(&w, "Hostname=%s\n", s.Branding.OSID)
	fmt.Fprintf(&w, "Locale=%s\nKeymap=%s\nTimezone=%s\n", s.Localization.Locale, s.Localization.Keymap, s.Localization.Timezone)
	w.WriteString("Packages=")
	pk := make([]string, 0, len(c.pkgs))
	for p := range c.pkgs {
		pk = append(pk, p)
	}
	sort.Strings(pk)
	w.WriteString(strings.Join(pk, "\n         "))
	w.WriteString("\n")
	fmt.Fprintf(&w, "KernelCommandLine=%s\n", strings.Join(c.cmdline, " "))
	// Only the mkosi-initrd default module set (storage, filesystems, dm-verity,
	// dm-crypt, TPM, input): keeps the UKI small and fast to load.
	w.WriteString("KernelInitrdModules=default\n")
	if len(c.extraTrees) > 0 {
		fmt.Fprintf(&w, "ExtraTrees=%s\n", strings.Join(c.extraTrees, ","))
	}
	w.WriteString("RemoveFiles=/var/cache/apt /var/lib/apt/lists /usr/share/doc/*/changelog*\n\n")

	w.WriteString("[Validation]\nSecureBoot=no\nVerity=defer\n")
	c.t.add("mkosi.conf", 0o644, w.String())
}

func (c *ctx) partitions() {
	sl := Slots(c.s.Foundation.Profile)
	l := c.labels
	c.t.add("mkosi.repart/00-esp.conf", 0o644, "[Partition]\nType=esp\nFormat=vfat\nCopyFiles=/boot:/\nCopyFiles=/efi:/\nSizeMinBytes=1G\nSizeMaxBytes=1G\n")
	c.t.add("mkosi.repart/10-usr.conf", 0o644, fmt.Sprintf(
		"[Partition]\nType=usr\nFormat=erofs\nCopyFiles=/usr:/\nVerity=data\nVerityMatchKey=usr\nSplitName=usr\nCompression=lz4hc\nSizeMinBytes=%d\nSizeMaxBytes=%d\nLabel=%s\n", sl.USR, sl.USR, l.USR))
	c.t.add("mkosi.repart/11-usr-verity.conf", 0o644, fmt.Sprintf(
		"[Partition]\nType=usr-verity\nVerity=hash\nVerityMatchKey=usr\nSplitName=usr-verity\nSizeMinBytes=%d\nSizeMaxBytes=%d\nLabel=%s\n", sl.Verity, sl.Verity, l.Verity))
	c.t.add("mkosi.repart/12-usr-verity-sig.conf", 0o644, fmt.Sprintf(
		"[Partition]\nType=usr-verity-sig\nVerity=signature\nVerityMatchKey=usr\nSplitName=usr-verity-sig\nLabel=%s\n", l.Sig))
	c.t.add("mkosi.repart/20-usr-b.conf", 0o644, fmt.Sprintf("[Partition]\nType=usr\nLabel=_empty\nSizeMinBytes=%d\nSizeMaxBytes=%d\n", sl.USR, sl.USR))
	c.t.add("mkosi.repart/21-usr-b-verity.conf", 0o644, fmt.Sprintf("[Partition]\nType=usr-verity\nLabel=_empty\nSizeMinBytes=%d\nSizeMaxBytes=%d\n", sl.Verity, sl.Verity))
	c.t.add("mkosi.repart/22-usr-b-verity-sig.conf", 0o644, "[Partition]\nType=usr-verity-sig\nLabel=_empty\nSizeMinBytes=16K\nSizeMaxBytes=16K\n")
}

// initrd: first-boot root creation (encrypted when required) and no shells.
func (c *ctx) initrd() {
	sec := c.s.Security
	base := "mkosi.initrd.conf/mkosi.extra/usr/lib/"
	root := "[Partition]\nType=root\nFormat=ext4\nFactoryReset=yes\nCopyFiles=/usr/share/foundry/rootfs:/\n"
	plain := root
	if sec.DiskEncryption != "none" {
		root += "Encrypt=tpm2\n"
	}
	c.t.add(base+"repart.d/50-root.conf", 0o644, root)
	c.t.add(base+"foundry/repart-plain.d/50-root.conf", 0o644, plain)
	policy := "require-tpm"
	if sec.Level == "relaxed" || c.in.Resolution.Virtual || sec.DiskEncryption == "none" {
		policy = "allow-unencrypted"
	}
	c.t.add(base+"foundry/encryption-policy", 0o644, policy+"\n")
	c.t.add(base+"foundry/initrd-repart", 0o755, embedded("initrd/initrd-repart"))
	// Live/installer boots (signed installer UKI): RAM root filled from the image.
	c.t.add(base+"foundry/live-root", 0o755, embedded("initrd/live-root"))
	c.t.add(base+"systemd/system/foundry-live-root.service", 0o644, `[Unit]
Description=Foundry live root
DefaultDependencies=no
ConditionKernelCommandLine=foundry.installer=1
After=sysroot.mount sysroot-usr.mount sysusr-usr.mount
Requires=sysroot.mount
Before=initrd-parse-etc.service initrd-fs.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/lib/foundry/live-root
StandardOutput=journal+console
`)
	c.t.symlink(base+"systemd/system/initrd-fs.target.wants/foundry-live-root.service", "../foundry-live-root.service")
	// Live media booted as a CD/DVD: the kernel does not scan partitions on
	// optical drives, so attach the disc to a partition-scanning loop device;
	// the signed /usr partition then appears under its PARTUUID as usual.
	c.t.add(base+"udev/rules.d/90-foundry-optical.rules", 0o644,
		"ACTION==\"add\", SUBSYSTEM==\"block\", KERNEL==\"sr[0-9]*\", ENV{ID_CDROM_MEDIA}!=\"0\", TAG+=\"systemd\", ENV{SYSTEMD_WANTS}+=\"foundry-optical@%k.service\"\n")
	c.t.add(base+"systemd/system/foundry-optical@.service", 0o644, `[Unit]
Description=Attach live medium %I
DefaultDependencies=no
ConditionKernelCommandLine=foundry.installer=1
BindsTo=dev-%i.device
After=dev-%i.device

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/losetup --find --partscan --read-only --sector-size 512 /dev/%I
`)
	c.t.add(base+"systemd/system/systemd-repart.service.d/10-foundry.conf", 0o644,
		"[Service]\nExecStart=\nExecStart=/usr/lib/foundry/initrd-repart\nStandardOutput=journal+console\nStandardError=journal+console\n")
	if c.s.Access.RootModel != "developer" {
		reboot := "[Service]\nExecStart=\nExecStart=/usr/bin/systemctl --no-block reboot\n"
		for _, u := range []string{"emergency", "rescue"} {
			c.t.add(base+"systemd/system/"+u+".service.d/10-foundry-reboot.conf", 0o644, reboot)
			c.t.add("mkosi.extra/usr/lib/systemd/system/"+u+".service.d/10-foundry-reboot.conf", 0o644, reboot)
		}
	}
}

// network: NetworkManager on graphical profiles, systemd-networkd (DHCP on
// every wired NIC) on headless ones. Units we don't use are disabled so they
// never show up as failed (which would fail the boot health check).
func (c *ctx) network() {
	c.disable("systemd-pcrlock-file-system.service", "systemd-pcrlock-firmware-code.service",
		"systemd-pcrlock-firmware-config.service", "systemd-pcrlock-machine-id.service",
		"systemd-pcrlock-make-policy.service", "systemd-pcrlock-secureboot-authority.service",
		"systemd-pcrlock-secureboot-policy.service")
	if c.graphical() {
		c.enable("NetworkManager.service")
		c.disable("systemd-networkd.service", "systemd-networkd-wait-online.service")
		return
	}
	c.t.add(extra+"usr/lib/systemd/network/80-foundry-wired.network", 0o644,
		"[Match]\nType=ether\nKind=!*\n\n[Network]\nDHCP=yes\nIPv6AcceptRA=yes\n\n[DHCPv4]\nUseDomains=yes\n")
	c.enable("systemd-networkd.service", "systemd-resolved.service")
	c.t.add(extra+"usr/lib/systemd/system/systemd-networkd-wait-online.service.d/10-foundry.conf", 0o644,
		"[Service]\nExecStart=\nExecStart=/usr/lib/systemd/systemd-networkd-wait-online --any --timeout=30\n")
}

func (c *ctx) branding() {
	s, b := c.s, c.base
	var w strings.Builder
	q := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&w, "%s=%q\n", k, v)
		}
	}
	q("NAME", s.Branding.OSName)
	q("PRETTY_NAME", s.Branding.OSName+" "+s.Metadata.Version)
	fmt.Fprintf(&w, "ID=%s\n", s.Branding.OSID)
	q("ID_LIKE", b.IDLike)
	q("VERSION", s.Metadata.Version)
	fmt.Fprintf(&w, "VERSION_ID=%s\n", s.Metadata.Version)
	q("HOME_URL", s.Branding.HomeURL)
	q("SUPPORT_URL", s.Branding.SupportURL)
	q("FOUNDRY_BASE", b.ID)
	q("FOUNDRY_SPEC_DIGEST", c.in.SpecDigest)
	c.t.add("mkosi.extra/usr/lib/os-release", 0o644, w.String())
	c.values["osName"] = s.Branding.OSName
}

func (c *ctx) presetFile() {
	if len(c.presets) == 0 {
		return
	}
	lines := append([]string(nil), c.presets...)
	sort.SliceStable(lines, func(i, j int) bool { return lines[i] < lines[j] })
	c.t.add("mkosi.extra/usr/lib/systemd/system-preset/10-foundry.preset", 0o644, "# Generated by Foundry\n"+strings.Join(lines, "\n")+"\n")
}

func (c *ctx) enable(units ...string) {
	for _, u := range units {
		c.presets = append(c.presets, "enable "+u)
	}
}

func (c *ctx) disable(units ...string) {
	for _, u := range units {
		c.presets = append(c.presets, "disable "+u)
	}
}

// valuesFile exposes spec-derived values to fixed postinst scripts as JSON
// (user values never become shell source).
func (c *ctx) valuesFile() {
	b, _ := json.MarshalIndent(c.values, "", "  ")
	c.t.add("mkosi.extra/usr/lib/foundry/values.json", 0o644, string(b)+"\n")
}

func (c *ctx) postinst() {
	var w strings.Builder
	w.WriteString("#!/bin/bash\n# Generated by Foundry compiler. Fixed text only; user values come from values.json.\n")
	w.WriteString("set -euo pipefail\nexport LC_ALL=C.UTF-8 TZ=UTC\n\n")
	w.WriteString(postKernel)
	for _, p := range c.post {
		w.WriteString("\n")
		w.WriteString(p)
	}
	w.WriteString("\n")
	w.WriteString(pkgSnapshot)
	for _, p := range c.postLate {
		w.WriteString("\n")
		w.WriteString(p)
	}
	c.t.add("mkosi.postinst.chroot", 0o755, w.String())
	c.t.add("mkosi.finalize.chroot", 0o755, "#!/bin/bash\n# Generated by Foundry compiler. Runs after presets and sysusers.\nset -euo pipefail\nexport LC_ALL=C.UTF-8\n\n"+postRootfs)
}

// postKernel: Ubuntu/Debian keep vmlinuz in /boot; mkosi expects it next to the modules.
const postKernel = `# Place the kernel where UKI tooling expects it.
for f in /boot/vmlinuz-*; do
  [ -L "$f" ] && continue
  k=${f#/boot/vmlinuz-}
  if [ -d "/usr/lib/modules/$k" ]; then cp "$f" "/usr/lib/modules/$k/vmlinuz"; fi
done
`

// pkgSnapshot records which package owns every file, with its checksum, for
// the security scanner — taken after the platform's own steps and before any
// customer content runs. Customer scripts cannot reach $OUTPUTDIR (they run
// in their own mount namespace without /work); the digest is also logged,
// so later tampering is detectable either way.
const pkgSnapshot = `# Package file database snapshot for the scanner (before customer content).
out=${OUTPUTDIR:-/work/out}
if [ -d "$out" ]; then
  {
    for f in /var/lib/dpkg/info/*.md5sums; do
      p=$(basename "$f" .md5sums); p=${p%%:*}
      awk -v p="$p" 'length($1) == 32 { print $1 "  /" substr($0, 35) "\t" p }' "$f"
    done
    dpkg-query -W -f='${Package}\n${Conffiles}\n' | awk '/^[^ ]/ { p = $1; next } NF >= 2 && $1 ~ /^\// && length($2) == 32 { print $2 "  " $1 "\t" p }'
    # Baseline of everything else present now (maintainer-script output such as
    # alternatives, PAM stacks, depmod files and the .wants symlinks packages
    # ship): the scanner flags only what appears or changes after this point.
    find /usr /etc -xdev -type f -print0 | xargs -0 -r md5sum |
      awk 'length($1) == 32 && substr($0, 33, 2) == "  " && index($0, "\t") == 0 { print $0 "\t@base" }'
    find /usr /etc -xdev -type l -printf '@link\t%p\t%l\n'
  } > "$out/foundry-dpkg.md5sums"
  echo "foundry: package snapshot sha256 $(sha256sum < "$out/foundry-dpkg.md5sums" | cut -d' ' -f1)"
fi
`

// postRootfs builds the root file system skeleton copied onto the (encrypted)
// root partition at first boot. /etc and /var/lib come from the build.
const postRootfs = `# First-boot root skeleton (see docs/context/boot-firmware-and-disk.md §11).
F=/usr/share/foundry/rootfs
rm -rf "$F"; mkdir -p "$F"
for d in boot efi home mnt media opt srv root tmp var proc sys dev run usr; do mkdir -p "$F/$d"; done
chmod 1777 "$F/tmp"; chmod 0700 "$F/root"
for l in bin sbin lib lib32 lib64 libx32; do if [ -L "/$l" ]; then cp -a "/$l" "$F/"; fi; done
cp -a /etc "$F/etc"
mkdir -p "$F/var"; cp -a /var/lib "$F/var/lib"
rm -rf "$F/var/lib/apt" "$F/var/lib/dpkg/info" "$F/etc/machine-id"
echo uninitialized > "$F/etc/machine-id"
# Kernels live in the UKI; keep the ESP free of stray /boot copies.
rm -f /boot/vmlinuz* /boot/initrd.img* /boot/System.map-* /boot/config-*
`
