package compiler

import (
	"fmt"
	"strings"
	"time"
)

// Base describes how to build one upstream distribution release.
type Base struct {
	ID           string
	Distribution string // mkosi Distribution=
	Release      string // mkosi Release=
	Repositories []string
	Mirror       string // default mirror (unpinned)
	SnapshotURL  string // printf pattern taking a UTC timestamp YYYYMMDDTHHMMSSZ; "" = unsupported
	IDLike       string
	Systemd      int // systemd major version shipped (drives feature use)
	Packages     []string
	GraphicalPkg []string
	GNOMEAccent  bool // GNOME ≥ 47 supports org.gnome.desktop.interface accent-color
	Tested       bool // built and boot-tested in CI in this environment
}

var bases = map[string]Base{
	"ubuntu-24.04": {
		ID: "ubuntu-24.04", Distribution: "ubuntu", Release: "noble",
		Repositories: []string{"main", "universe"},
		Mirror:       "http://archive.ubuntu.com/ubuntu",
		SnapshotURL:  "https://snapshot.ubuntu.com/ubuntu/%s",
		IDLike:       "ubuntu debian", Systemd: 255,
		Packages: []string{
			"systemd", "systemd-sysv", "systemd-boot", "systemd-boot-efi", "systemd-resolved", "systemd-timesyncd",
			"systemd-oomd", "systemd-zram-generator", "udev", "dbus", "kmod", "cryptsetup-bin", "tpm-udev",
			"libtss2-esys-3.0.2-0t64", "libtss2-rc0t64", "libtss2-mu-4.0.1-0t64", "libtss2-tcti-device0t64",
			"iproute2", "ca-certificates", "locales", "tzdata", "less", "nftables", "python3",
		},
		GraphicalPkg: []string{"plymouth", "plymouth-label", "network-manager", "dconf-cli", "fonts-dejavu-core"},
		Tested:       true,
	},
	"ubuntu-26.04": {
		ID: "ubuntu-26.04", Distribution: "ubuntu", Release: "resolute",
		Repositories: []string{"main", "universe"},
		Mirror:       "http://archive.ubuntu.com/ubuntu",
		SnapshotURL:  "https://snapshot.ubuntu.com/ubuntu/%s",
		IDLike:       "ubuntu debian", Systemd: 259,
		Packages: []string{
			"systemd", "systemd-sysv", "systemd-boot", "systemd-boot-efi", "systemd-repart", "systemd-resolved",
			"systemd-timesyncd", "systemd-oomd", "systemd-zram-generator", "systemd-cryptsetup", "udev", "dbus", "kmod",
			"cryptsetup-bin", "tpm-udev", "iproute2", "ca-certificates", "locales", "tzdata", "less", "nftables", "python3",
		},
		GraphicalPkg: []string{"plymouth", "plymouth-label", "network-manager", "dconf-cli", "fonts-dejavu-core"},
		GNOMEAccent:  true,
	},
	"debian-13": {
		ID: "debian-13", Distribution: "debian", Release: "trixie",
		Repositories: []string{"main", "non-free-firmware"},
		Mirror:       "http://deb.debian.org/debian",
		SnapshotURL:  "https://snapshot.debian.org/archive/debian/%s/",
		IDLike:       "debian", Systemd: 257,
		Packages: []string{
			"systemd", "systemd-sysv", "systemd-boot", "systemd-boot-efi", "systemd-repart", "systemd-resolved",
			"systemd-timesyncd", "systemd-oomd", "systemd-zram-generator", "systemd-cryptsetup", "udev", "dbus", "kmod",
			"cryptsetup-bin", "tpm-udev", "iproute2", "ca-certificates", "locales", "tzdata", "less", "nftables", "python3",
		},
		GraphicalPkg: []string{"plymouth", "plymouth-label", "network-manager", "dconf-cli", "fonts-dejavu-core"},
		GNOMEAccent:  false,
	},
}

// LookupBase returns the base definition.
func LookupBase(id string) (Base, error) {
	b, ok := bases[id]
	if !ok {
		return Base{}, fmt.Errorf("compiler: base %q is not supported by this compiler", id)
	}
	return b, nil
}

// SupportedBases lists bases the compiler can produce.
func SupportedBases() []string { return []string{"ubuntu-24.04", "ubuntu-26.04", "debian-13"} }

// snapshotStamp converts an RFC 3339 time to the mirror's path format.
func snapshotStamp(rfc3339 string) (string, int64, error) {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return "", 0, err
	}
	t = t.UTC()
	return strings.ReplaceAll(t.Format("20060102T150405Z"), ":", ""), t.Unix(), nil
}
