package upload

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

var debNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]{1,62}$`)

// ValidateDeb reads a .deb's control fields with dpkg-deb. The package name
// is recorded so the compiler can install it by name from the local repo.
func ValidateDeb(ctx context.Context, path string) *Report {
	rep := &Report{Kind: "deb"}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "dpkg-deb", "--field", path, "Package", "Version", "Architecture").Output()
	if err != nil {
		rep.add("", "UPL-DEB-001", "not a valid Debian package")
		return rep
	}
	f := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), ":"); ok {
			f[k] = strings.TrimSpace(v)
		}
	}
	rep.Format = f["Package"] + " " + f["Version"] + " " + f["Architecture"]
	rep.Package = f["Package"]
	if !debNameRe.MatchString(f["Package"]) {
		rep.add("", "UPL-DEB-002", "invalid package name")
	}
	switch f["Architecture"] {
	case "amd64", "arm64", "all":
	default:
		rep.add("", "UPL-DEB-003", "unsupported architecture "+f["Architecture"])
	}
	// Maintainer scripts run as root during the build: list them for review.
	if out, err := exec.CommandContext(ctx, "dpkg-deb", "--ctrl-tarfile", path).Output(); err == nil {
		if r, err := ValidateArchive(bytes.NewReader(out), DefaultLimits); err == nil {
			rep.Scripts = r.Scripts
		}
	}
	rep.Valid = len(rep.Findings) == 0
	return rep
}

// ValidateGPGKey computes the fingerprint of a single OpenPGP public key.
func ValidateGPGKey(ctx context.Context, path string) *Report {
	rep := &Report{Kind: "gpg-key"}
	home, err := os.MkdirTemp("", "gpg-")
	if err != nil {
		rep.add("", "UPL-KEY-000", err.Error())
		return rep
	}
	defer os.RemoveAll(home)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gpg", "--homedir", home, "--batch", "--with-colons", "--show-keys", path).Output()
	if err != nil {
		rep.add("", "UPL-KEY-001", "not an OpenPGP public key")
		return rep
	}
	var fprs []string
	primary := false
	for _, l := range strings.Split(string(out), "\n") {
		fs := strings.Split(l, ":")
		switch {
		case fs[0] == "sec":
			rep.add("", "UPL-KEY-002", "file contains a secret key")
		case fs[0] == "pub":
			primary = true
		case fs[0] == "fpr" && primary && len(fs) > 9:
			fprs = append(fprs, fs[9])
			primary = false
		}
	}
	if len(fprs) != 1 {
		rep.add("", "UPL-KEY-003", fmt.Sprintf("expected exactly one key, found %d", len(fprs)))
	} else {
		rep.Fingerprint = fprs[0]
	}
	rep.Valid = len(rep.Findings) == 0
	return rep
}

// mkosiAllowed lists the mkosi settings an expert drop-in may set. Anything
// touching boot, verity, signing, output, tools or host paths is refused.
var mkosiAllowed = map[string]map[string]bool{
	"Content": {"Packages": true, "RemovePackages": true, "RemoveFiles": true, "Locale": true, "LocaleMessages": true,
		"Keymap": true, "Timezone": true, "Hostname": true, "WithDocs": true, "Environment": true, "FirmwareFiles": true, "KernelModules": true},
	"Distribution": {},
	"Build":        {},
}

// ValidateMkosiDropin parses an INI drop-in and enforces the allowlist.
func ValidateMkosiDropin(data []byte) *Report {
	rep := &Report{Kind: "mkosi-dropin"}
	section := ""
	for n, raw := range strings.Split(string(data), "\n") {
		l := strings.TrimSpace(raw)
		if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, ";") {
			continue
		}
		if strings.HasPrefix(l, "[") && strings.HasSuffix(l, "]") {
			section = strings.TrimSuffix(strings.TrimPrefix(l, "["), "]")
			if _, ok := mkosiAllowed[section]; !ok {
				rep.add(fmt.Sprintf("line %d", n+1), "UPL-MKOSI-001", "section ["+section+"] is not allowed")
			}
			continue
		}
		k, _, ok := strings.Cut(l, "=")
		if !ok {
			if section == "Content" { // continuation lines of a list setting
				continue
			}
			rep.add(fmt.Sprintf("line %d", n+1), "UPL-MKOSI-002", "not a key=value line")
			continue
		}
		k = strings.TrimSpace(k)
		if !mkosiAllowed[section][k] {
			rep.add(fmt.Sprintf("line %d", n+1), "UPL-MKOSI-003", fmt.Sprintf("setting %s= in [%s] is not allowed in expert drop-ins", k, section))
		}
	}
	rep.Valid = len(rep.Findings) == 0
	return rep
}
