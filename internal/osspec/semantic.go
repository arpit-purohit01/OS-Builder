package osspec

import (
	"fmt"
	"regexp"
	"strings"
)

// Semantic rule codes are stable; UI and docs refer to them.
const (
	SemSecureBootOffNeedsRelaxed = "SEM-001"
	SemLockedNoTTY               = "SEM-002"
	SemTPMPinNotKiosk            = "SEM-004"
	SemKioskNeedsURL             = "SEM-005"
	SemKioskSectionProfile       = "SEM-006"
	SemAutologinKiosk            = "SEM-007"
	SemIdentityFields            = "SEM-008"
	SemSyncURL                   = "SEM-009"
	SemCmdlineDenied             = "SEM-010"
	SemHardenedNeedsProtection   = "SEM-011"
	SemDeveloperNotHardened      = "SEM-012"
	SemTrademark                 = "SEM-013"
	SemBiosCompatX86Only         = "SEM-014"
	SemPredefinedUsers           = "SEM-015"
	SemMultiArchUnsupported      = "SEM-016"
	SemExtraRepo                 = "SEM-017"
	SemSSHExposure               = "SEM-W01" // warning
	SemUSBAllowedHardened        = "SEM-W02" // warning
)

// CmdlineAllowPrefixes are kernel parameters accepted in expert.extraCmdline.
var CmdlineAllowPrefixes = []string{
	"quiet", "splash", "loglevel=", "video=", "i915.", "amdgpu.", "nouveau.", "nvme_core.default_ps_max_latency_us=",
	"pcie_aspm=", "mem_sleep_default=", "acpi_backlight=", "fbcon=", "consoleblank=", "usbcore.autosuspend=",
}

// CmdlineDenyPrefixes are rejected even if they also match an allow prefix.
var CmdlineDenyPrefixes = []string{
	"init=", "rd.break", "rd.shell", "systemd.unit=", "systemd.debug_shell", "lockdown=", "module.sig_enforce=",
	"selinux=", "apparmor=", "security=", "usrhash=", "roothash=", "rw", "single", "emergency", "rescue",
	"nokaslr", "mitigations=off", "iommu=off", "efi=", "systemd.confirm_spawn",
}

var trademarkDenylist = []string{"debian", "ubuntu", "fedora", "windows", "macos", "ios", "android", "redhat", "red hat", "microsoft", "apple", "chromeos", "kali", "zorin"}

// Semantic runs cross-field rules (layer 2). Returns errors and warnings.
var pinRe = regexp.MustCompile(`^(origin|release|version) [A-Za-z0-9=.,:_*+~ -]{1,90}$`)

func Semantic(s *Spec) (errs, warns []FieldError) {
	add := func(ptr, code, msg string) { errs = append(errs, FieldError{Pointer: ptr, Code: code, Message: msg}) }
	warn := func(ptr, code, msg string) { warns = append(warns, FieldError{Pointer: ptr, Code: code, Message: msg}) }

	sec, acc, eco := s.Security, s.Access, s.Ecosystem
	kiosk := s.Foundation.Profile == "kiosk-cage" || s.Foundation.Profile == "thin-client"

	if sec.SecureBoot == "off" && sec.Level != "relaxed" {
		add("/security/secureBoot", SemSecureBootOffNeedsRelaxed, "Secure Boot can only be turned off at the 'relaxed' security level.")
	}
	if acc.RootModel == "locked" && acc.TTYConsole != nil && *acc.TTYConsole {
		add("/access/ttyConsole", SemLockedNoTTY, "A locked system cannot offer text consoles.")
	}
	if sec.DiskEncryption == "tpm2+pin" && kiosk {
		add("/security/diskEncryption", SemTPMPinNotKiosk, "Kiosks start unattended, so they cannot ask for a disk PIN. Use 'tpm2'.")
	}
	if kiosk && (eco.Kiosk == nil || eco.Kiosk.URL == "") {
		add("/ecosystem/kiosk/url", SemKioskNeedsURL, "A kiosk or thin client needs the address it should open.")
	}
	if !kiosk && eco.Kiosk != nil && (eco.Kiosk.URL != "" || len(eco.Kiosk.AllowedDomains) > 0 || eco.Kiosk.IdleResetMinutes > 0) {
		add("/ecosystem/kiosk", SemKioskSectionProfile, "Kiosk settings only apply to the kiosk and thin-client profiles.")
	}
	if (acc.Users.Mode == "autologin-kiosk") != kiosk {
		if kiosk {
			add("/access/users/mode", SemAutologinKiosk, "Kiosks and thin clients always sign in automatically; set users mode to 'autologin-kiosk'.")
		} else {
			add("/access/users/mode", SemAutologinKiosk, "Automatic kiosk sign-in is only available with the kiosk and thin-client profiles.")
		}
	}
	switch id := eco.Identity; id.Provider {
	case "oidc":
		if id.Issuer == "" || id.ClientID == "" {
			add("/ecosystem/identity", SemIdentityFields, "Single sign-on needs the identity provider address (issuer) and client ID.")
		}
	case "ldap", "ad":
		if id.Domain == "" {
			add("/ecosystem/identity/domain", SemIdentityFields, "Directory sign-in needs the domain name.")
		}
	}
	if acc.Users.Mode == "directory" && eco.Identity.Provider == "local" {
		add("/access/users/mode", SemIdentityFields, "Directory accounts need an identity provider (OIDC, LDAP or AD).")
	}
	if eco.Sync.Provider == "webdav" && eco.Sync.URL == "" {
		add("/ecosystem/sync/url", SemSyncURL, "WebDAV sync needs a server address.")
	}
	for i, p := range s.Kernel.ExtraCmdline {
		if !CmdlineAllowed(p) {
			add(fmt.Sprintf("/kernel/extraCmdline/%d", i), SemCmdlineDenied, fmt.Sprintf("Kernel parameter %q is not allowed.", p))
		}
	}
	if s.Expert != nil {
		for i, r := range s.Expert.ExtraRepos {
			ptr := fmt.Sprintf("/expert/extraRepos/%d", i)
			if !strings.HasPrefix(r.URL, "https://") {
				add(ptr+"/url", SemExtraRepo, "Extra repositories must use HTTPS.")
			}
			if !pinRe.MatchString(r.Pin) {
				add(ptr+"/pin", SemExtraRepo, `Pin must look like "origin repo.example.com" or "release o=Example".`)
			}
		}
	}
	if sec.Level == "hardened" {
		if sec.SecureBoot == "off" || sec.DiskEncryption == "none" {
			add("/security/level", SemHardenedNeedsProtection, "The hardened level requires Secure Boot and disk encryption.")
		}
		if sec.USBStorage == "allow" {
			warn("/security/usbStorage", SemUSBAllowedHardened, "Hardened systems usually block or restrict USB storage.")
		}
	}
	if acc.RootModel == "developer" && sec.Level == "hardened" {
		add("/access/rootModel", SemDeveloperNotHardened, "Developer root access is not allowed at the hardened level.")
	}
	for _, f := range []struct{ ptr, v string }{
		{"/branding/osName", s.Branding.OSName}, {"/branding/osId", s.Branding.OSID},
		{"/metadata/displayName", s.Metadata.DisplayName}, {"/metadata/name", s.Metadata.Name},
	} {
		l := strings.ToLower(f.v)
		for _, tm := range trademarkDenylist {
			if containsWord(l, tm) {
				add(f.ptr, SemTrademark, fmt.Sprintf("Names may not include the trademark %q.", tm))
				break
			}
		}
	}
	if s.Foundation.BootMode == "uefi+bios-compat" {
		for _, a := range s.Foundation.Arch {
			if a != "x86_64" {
				add("/foundation/bootMode", SemBiosCompatX86Only, "Legacy BIOS compatibility exists only on x86_64.")
			}
		}
	}
	if acc.Users.Mode == "predefined" {
		if len(acc.Users.Predefined) == 0 {
			add("/access/users/predefined", SemPredefinedUsers, "Add at least one user.")
		}
		admins := 0
		for i, u := range acc.Users.Predefined {
			if u.Admin {
				admins++
				if acc.RootModel == "locked" {
					add(fmt.Sprintf("/access/users/predefined/%d/admin", i), SemPredefinedUsers, "A locked system cannot have administrator users.")
				}
			}
		}
		if admins == 0 && acc.RootModel != "locked" && len(acc.Users.Predefined) > 0 {
			add("/access/users/predefined", SemPredefinedUsers, "At least one predefined user must be an administrator.")
		}
	}
	if len(s.Foundation.Arch) > 1 {
		add("/foundation/arch", SemMultiArchUnsupported, "Build one architecture per spec version for now.")
	}
	if sec.Firewall != nil {
		for _, a := range sec.Firewall.Allow {
			if a == "ssh" {
				warn("/security/firewall/allow", SemSSHExposure, "Opening SSH exposes a remote login service.")
			}
		}
	}
	return errs, warns
}

// CmdlineAllowed reports whether one kernel parameter may be added by users.
func CmdlineAllowed(p string) bool {
	for _, d := range CmdlineDenyPrefixes {
		if p == d || strings.HasPrefix(p, d) && (strings.HasSuffix(d, "=") || strings.HasSuffix(d, ".") || strings.HasPrefix(p, d+"=")) {
			return false
		}
	}
	for _, a := range CmdlineAllowPrefixes {
		if p == a || strings.HasPrefix(p, a) && (strings.HasSuffix(a, "=") || strings.HasSuffix(a, ".")) {
			return true
		}
	}
	return false
}

func containsWord(s, w string) bool {
	idx := 0
	for {
		i := strings.Index(s[idx:], w)
		if i < 0 {
			return false
		}
		i += idx
		before := i == 0 || !isAlnum(s[i-1])
		end := i + len(w)
		after := end == len(s) || !isAlnum(s[end])
		if before && after {
			return true
		}
		idx = i + 1
	}
}

func isAlnum(c byte) bool { return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' }
