package scan

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"foundry.dev/foundry/internal/gpt"
	"foundry.dev/foundry/internal/imgutil"
)

// Image analysis (P3-04): a manifest of every file of the built /usr (the
// erofs partition, extracted in user space — the image is untrusted and is
// never mounted by the host kernel), checked against the package database
// snapshot the build took before any customer content ran.
//
// /usr/share/foundry/rootfs is the first-boot skeleton of /, so its etc/,
// root/, var/ entries are analysed as /etc, /root, /var.

// FileEntry is one manifest line.
type FileEntry struct {
	Path   string `json:"path"`
	Type   string `json:"type"` // f d l o
	Mode   uint32 `json:"mode"`
	UID    int    `json:"uid"`
	GID    int    `json:"gid"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
	MD5    string `json:"-"`
	Link   string `json:"link,omitempty"`
	ELF    bool   `json:"elf,omitempty"`
}

// Manifest is sorted by path.
type Manifest []FileEntry

// ExtractUsr unpacks slot A's usr partition of a raw disk.
// Extraction is always done on a native Linux filesystem (not 9p/WSL mount)
// to avoid erofs extraction failures on Windows filesystem.
func ExtractUsr(ctx context.Context, disk string) (string, Manifest, error) {
	t, err := gpt.ReadFile(disk)
	if err != nil {
		return "", nil, err
	}
	var usr *gpt.Partition
	for i := range t.Partitions {
		p := &t.Partitions[i]
		if (p.Type == gpt.TypeUsrX86_64 || p.Type == gpt.TypeUsrArm64) && p.Name != "_empty" {
			usr = p
		}
	}
	if usr == nil {
		return "", nil, fmt.Errorf("scan: no usr partition")
	}

	// Use native Linux temp directory for extraction (avoids 9p/WSL issues)
	tmpDir, err := os.MkdirTemp("", "foundry-scan-")
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(tmpDir)

	img := filepath.Join(tmpDir, "usr.erofs")
	if err := imgutil.ExtractRange(disk, usr.Offset(), usr.Size(), img); err != nil {
		return "", nil, err
	}
	defer os.Remove(img)

	root := filepath.Join(tmpDir, "usr")
	if err := imgutil.Run(ctx, nil, "fsck.erofs", "--extract="+root, "--preserve-perms", "--no-preserve-owner", img); err != nil {
		return "", nil, err
	}

	// Build manifest from extracted root
	m, err := BuildManifest(root, "")
	if err != nil {
		return "", nil, err
	}

	return root, m, nil
}

// BuildManifest walks an extracted /usr tree.
func BuildManifest(root, prefix string) (Manifest, error) {
	var m Manifest
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		e := FileEntry{Path: path.Clean(prefix + "/" + filepath.ToSlash(rel))}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		e.Mode = uint32(fi.Mode().Perm())
		if fi.Mode()&fs.ModeSetuid != 0 {
			e.Mode |= 0o4000
		}
		if fi.Mode()&fs.ModeSetgid != 0 {
			e.Mode |= 0o2000
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			e.UID, e.GID = int(st.Uid), int(st.Gid)
		}
		switch {
		case fi.Mode().IsRegular():
			e.Type, e.Size = "f", fi.Size()
			if err := hashFile(p, &e); err != nil {
				return err
			}
		case fi.IsDir():
			e.Type = "d"
		case fi.Mode()&fs.ModeSymlink != 0:
			e.Type = "l"
			e.Link, _ = os.Readlink(p)
		default:
			e.Type = "o"
		}
		m = append(m, e)
		return nil
	})
	sort.Slice(m, func(i, j int) bool { return m[i].Path < m[j].Path })
	return m, err
}

func hashFile(p string, e *FileEntry) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	s, m := sha256.New(), md5.New()
	head := make([]byte, 4)
	n, _ := io.ReadFull(f, head)
	e.ELF = n == 4 && string(head) == "\x7fELF"
	s.Write(head[:n])
	m.Write(head[:n])
	if _, err := io.Copy(io.MultiWriter(s, m), f); err != nil {
		return err
	}
	e.SHA256, e.MD5 = hex.EncodeToString(s.Sum(nil)), hex.EncodeToString(m.Sum(nil))
	return nil
}

// PkgFile is a package-owned file from the snapshot.
type PkgFile struct {
	MD5, Pkg string
	Base     string // md5 (files) or link target (symlinks) when the snapshot was taken
	IsLink   bool
}

// BasePkg owns files that no package lists but that existed when the
// snapshot was taken (maintainer-script output, shipped symlinks).
const BasePkg = "@base"

// Ref is the content the platform itself left behind: the baseline when one
// was recorded, else the package's checksum.
func (f PkgFile) Ref() string {
	if f.Base != "" {
		return f.Base
	}
	return f.MD5
}

// PkgDB maps absolute paths to their owning package.
type PkgDB map[string]PkgFile

// ParsePkgDB reads "<md5>  /path\t<package>" lines (foundry-dpkg.md5sums).
func ParsePkgDB(b []byte) (PkgDB, error) {
	db := PkgDB{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		l := sc.Text()
		if strings.HasPrefix(l, "@link\t") {
			f := strings.SplitN(l, "\t", 3)
			if len(f) == 3 {
				p := path.Clean("/" + strings.TrimPrefix(f[1], "/"))
				e := db[p]
				e.Base, e.IsLink = f[2], true
				if e.Pkg == "" {
					e.Pkg = BasePkg
				}
				db[p] = e
			}
			continue
		}
		tab := strings.LastIndexByte(l, '\t')
		if len(l) < 36 || tab < 0 || l[32:34] != "  " {
			continue
		}
		p := path.Clean("/" + strings.TrimPrefix(l[34:tab], "/"))
		e := db[p]
		if pkg := l[tab+1:]; pkg == BasePkg {
			e.Base = l[:32]
			if e.Pkg == "" {
				e.Pkg = BasePkg
			}
		} else {
			e.MD5, e.Pkg = l[:32], pkg
		}
		db[p] = e
	}
	return db, sc.Err()
}

// owner finds the package entry for a path, following merged-/usr aliases
// (packages may list /bin/x for what is /usr/bin/x).
func (db PkgDB) owner(p string) (PkgFile, bool) {
	if f, ok := db[p]; ok {
		return f, true
	}
	for _, d := range []string{"bin", "sbin", "lib", "lib32", "lib64", "libx32"} {
		if strings.HasPrefix(p, "/usr/"+d+"/") {
			if f, ok := db[strings.TrimPrefix(p, "/usr")]; ok {
				return f, true
			}
		}
	}
	return PkgFile{}, false
}

const skeleton = "/usr/share/foundry/rootfs"

// effective maps a manifest path to where it lives on the running system.
func effective(p string) string {
	if strings.HasPrefix(p, skeleton+"/") {
		return strings.TrimPrefix(p, skeleton)
	}
	return p
}

// generated lists files the build itself writes or legitimately rewrites
// (mkosi, the platform's fixed postinst, package maintainer scripts). Content
// rules still apply to them.
var generated = regexp.MustCompile(`^/(` + strings.Join([]string{
	`etc/(hostname|machine-id|locale\.conf|vconsole\.conf|localtime|timezone|default/locale|ld\.so\.cache|shells|subuid|subgid|subuid-|subgid-|passwd-|group-|shadow-|gshadow-|adjtime|mtab|resolv\.conf|issue|issue\.net|os-release|fstab|crypttab|kernel/.*|nftables\.conf)`,
	`etc/(passwd|group|shadow|gshadow)`, // regenerated by systemd-sysusers / useradd (content-checked below)
	`etc/ssl/certs/.*`, `etc/alternatives/.*`, `etc/dconf/db/.*`,
	`etc/systemd/system/(dbus|display-manager|sshd|syslog|default\.target)\.service`, `etc/systemd/system/default\.target`,
	`etc/sudoers\.d/10-foundry`, `etc/sddm\.conf\.d/10-foundry\.conf`, `etc/xdg/kdeglobals`, `etc/dconf/(profile|db/local\.d)/.*`,
	`etc/apparmor\.d/cache/.*`, `etc/udev/hwdb\.bin`, `etc/fonts/conf\.d/.*`, `etc/(rc\d\.d|init\.d)/.*`, `etc/apt/.*`,
	`usr/lib/os-release`, `usr/lib/locale/.*`, `usr/lib/modules/[^/]+/(modules\..*|vmlinuz)`, `usr/lib/udev/hwdb\.bin`,
	`usr/lib/foundry/.*`, `usr/share/foundry/.*`, `usr/lib/systemd/system-preset/10-foundry\.preset`,
	`usr/share/(mime|icons/[^/]+/icon-theme\.cache|glib-2\.0/schemas/gschemas\.compiled|applications/mimeinfo\.cache|info/dir|fonts/.*\.cache.*)`,
	`usr/lib/x86_64-linux-gnu/gdk-pixbuf-2\.0/.*/loaders\.cache`, `usr/lib/python3[^/]*/.*__pycache__/.*`, `usr/lib/python3/dist-packages/.*__pycache__/.*`,
	`var/lib/(dpkg|apt|systemd|polkit-1|ucf|pam|shells\.state|sgml-base|xml-core|dbus|AccountsService|plymouth|flatpak|foundry)/.*`, `var/cache/.*`, `var/log/.*`,
	`usr/share/plasma/shells/org\.kde\.plasma\.desktop/contents/updates/foundry-defaults\.js`, `usr/share/sddm/themes/breeze/theme\.conf\.user`,
	`usr/share/backgrounds/foundry/.*`, `usr/share/plymouth/themes/foundry/.*`,
}, "|") + `)$`)

// ImageInput is what AnalyzeImage needs.
type ImageInput struct {
	Manifest     Manifest
	PkgDB        PkgDB
	Expected     func(path string) bool // compiled tree files (mkosi.extra)
	CustomerPkgs map[string]bool        // packages uploaded by the customer
	Overlay      map[string]string      // effective path → overlay name
	Read         func(path string) ([]byte, error)
}

// sensitiveUnits are stock units whose enabling is a decision of its own
// (remote access and file sharing); the catalog or a review must account for
// them, so enabling one is never treated as platform output.
var sensitiveUnits = regexp.MustCompile(`^(ssh|sshd|telnet|rsh|rlogin|vnc|x11vnc|xrdp|smbd|nmbd|nfs-[a-z-]+|rpcbind|cockpit|docker|containerd|snapd|avahi-daemon|cups|bluetooth|apache2|nginx|mysql|postgresql|redis-server|openvpn|wg-quick)[@.]`)

// stockUnitLink recognizes the symlinks systemd presets and aliases create in
// /etc/systemd/system: enabling, masking or aliasing a unit that a base
// package ships. Units from customer packages, overlays or scripts (and
// sensitive services) are not covered.
func (in ImageInput) stockUnitLink(p string, e FileEntry) bool {
	if e.Type != "l" || !strings.HasPrefix(p, "/etc/systemd/system/") {
		return false
	}
	tgt := e.Link
	if tgt == "/dev/null" {
		return true
	}
	if !strings.HasPrefix(tgt, "/") {
		tgt = path.Join(path.Dir(p), tgt)
	}
	tgt = path.Clean(tgt)
	if strings.HasPrefix(tgt, "/lib/") {
		tgt = "/usr" + tgt
	}
	if !strings.HasPrefix(tgt, "/usr/lib/systemd/system/") || sensitiveUnits.MatchString(path.Base(tgt)) {
		return false
	}
	if in.Overlay[tgt] != "" {
		return false
	}
	if in.Expected != nil && in.Expected(tgt) { // a unit the platform compiler ships
		return true
	}
	o, ok := in.PkgDB.owner(tgt)
	return ok && !in.CustomerPkgs[o.Pkg]
}

// ImageSummary counts what the analysis saw.
type ImageSummary struct {
	Files, Owned, Modified, Unowned, Setuid, Generated int
}

// AnalyzeImage classifies every file of the image.
func AnalyzeImage(in ImageInput) ([]Finding, ImageSummary) {
	var out []Finding
	var sum ImageSummary
	add := func(f Finding) {
		if f.Weight == 0 {
			f.Weight = Weights[f.Feature]
		}
		out = append(out, f)
	}
	for _, e := range in.Manifest {
		if e.Type == "d" {
			continue
		}
		sum.Files++
		p := effective(e.Path)
		rel := strings.TrimPrefix(p, "/")
		rule := ClassifyPath(rel)
		owner, owned := in.PkgDB.owner(p)
		gen := generated.MatchString(p) || (in.Expected != nil && in.Expected(p)) || in.stockUnitLink(p, e)
		ov := in.Overlay[p]
		switch {
		case owned && e.Type == "l" && owner.IsLink && owner.Base != e.Link && !gen:
			sum.Modified++
			add(Finding{Rule: "DIFF-MODIFIED", Feature: "modified-binary", Severity: SevHigh, Path: p, Scanner: "diff",
				Detail: fmt.Sprintf("symbolic link was retargeted after installation (%s → %s)", owner.Base, e.Link)})
		case owned && e.Type == "f" && owner.Ref() != e.MD5 && !gen:
			sum.Modified++
			sev, feat := SevHigh, "modified-binary"
			if rule != nil || strings.HasPrefix(p, "/usr/bin/") || strings.HasPrefix(p, "/usr/sbin/") || strings.HasPrefix(p, "/usr/lib/") {
				sev = SevBlocker
			}
			add(Finding{Rule: "DIFF-MODIFIED", Feature: feat, Severity: sev, Path: p, Scanner: "diff",
				Detail: fmt.Sprintf("file of package %s was changed after installation", owner.Pkg)})
		case owned && owner.Pkg == BasePkg:
			sum.Generated++
		case owned:
			sum.Owned++
			if in.CustomerPkgs[owner.Pkg] && rule != nil {
				add(Finding{Rule: "DIFF-CUSTOMPKG", Feature: rule.Feature, Severity: rule.Severity, Path: p, Scanner: "diff",
					Detail: fmt.Sprintf("uploaded package %s installs %s", owner.Pkg, rule.Detail)})
			}
		case gen:
			sum.Generated++
		default:
			sum.Unowned++
			src := "added by the build (script or overlay)"
			if ov != "" {
				src = "from overlay " + ov
			}
			if rule != nil {
				add(Finding{Rule: "DIFF-UNOWNED", Feature: rule.Feature, Severity: rule.Severity, Path: p, Scanner: "diff",
					Detail: rule.Detail + ": " + src})
			} else if e.ELF && ov == "" {
				add(Finding{Rule: "DIFF-ELF", Feature: "uploaded-elf", Severity: SevMedium, Path: p, Scanner: "diff",
					Detail: "executable not installed by any package: " + src})
			}
		}
		if e.Type == "f" && e.Mode&0o6000 != 0 {
			sum.Setuid++
			if !owned || in.CustomerPkgs[owner.Pkg] {
				add(Finding{Rule: "DIFF-SUID", Feature: "setuid", Severity: SevHigh, Path: p, Scanner: "diff",
					Detail: "setuid/setgid file not shipped by a base package"})
			}
		}
		// Content rules on everything not installed by a base package (and on
		// generated files whose content may carry customer values).
		if e.Type == "f" && e.Size <= 8<<20 && in.Read != nil && (!owned || in.CustomerPkgs[owner.Pkg] || owner.Ref() != e.MD5) {
			if b, err := in.Read(e.Path); err == nil {
				for _, f := range ScanContent(p, b) {
					add(f)
				}
			}
		}
	}
	return out, sum
}

// Diff compares two manifests (sorted) with a merge join: added, removed
// and changed paths (content, mode, owner or link target).
func Diff(a, b Manifest) (added, removed, changed []string) {
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case j == len(b) || (i < len(a) && a[i].Path < b[j].Path):
			removed = append(removed, a[i].Path)
			i++
		case i == len(a) || b[j].Path < a[i].Path:
			added = append(added, b[j].Path)
			j++
		default:
			x, y := a[i], b[j]
			if x.Type != y.Type || x.SHA256 != y.SHA256 || x.Mode != y.Mode || x.UID != y.UID || x.GID != y.GID || x.Link != y.Link {
				changed = append(changed, y.Path)
			}
			i++
			j++
		}
	}
	return
}
