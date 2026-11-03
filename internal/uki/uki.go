// Package uki reads Unified Kernel Images (PE files with .linux, .initrd,
// .cmdline, .osrel, .uname, .pcrsig, ... sections) and predicts the PCR 11
// values systemd-stub will measure. No key material is involved.
package uki

import (
	"context"
	"debug/pe"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Sections returns section payloads trimmed to their virtual size.
func Sections(path string) (map[string][]byte, error) {
	f, err := pe.Open(path)
	if err != nil {
		return nil, fmt.Errorf("uki: not a PE image: %w", err)
	}
	defer f.Close()
	out := map[string][]byte{}
	for _, s := range f.Sections {
		d, err := s.Data()
		if err != nil {
			return nil, err
		}
		if n := int(s.VirtualSize); n > 0 && n < len(d) {
			d = d[:n]
		}
		out[s.Name] = d
	}
	if len(out[".linux"]) == 0 {
		return nil, errors.New("uki: no .linux section")
	}
	return out, nil
}

var usrhashRe = regexp.MustCompile(`(?:^|\s)usrhash=([0-9a-f]{64})(?:\s|$)`)

// Info describes a UKI's identity-relevant content.
type Info struct {
	Cmdline string `json:"cmdline"`
	Usrhash string `json:"usrhash"`
	Uname   string `json:"uname"`
	OSRel   string `json:"osRelease"`
}

func trim(b []byte) string { return strings.TrimRight(string(b), "\x00\n ") }

// Inspect parses .cmdline/.uname/.osrel.
func Inspect(path string) (*Info, error) {
	s, err := Sections(path)
	if err != nil {
		return nil, err
	}
	info := &Info{Cmdline: trim(s[".cmdline"]), Uname: trim(s[".uname"]), OSRel: trim(s[".osrel"])}
	if m := usrhashRe.FindStringSubmatch(info.Cmdline); m != nil {
		info.Usrhash = m[1]
	}
	return info, nil
}

// measured sections in systemd-stub order and their systemd-measure flags.
var measured = []struct{ section, flag string }{
	{".linux", "--linux"}, {".osrel", "--osrel"}, {".cmdline", "--cmdline"}, {".initrd", "--initrd"},
	{".splash", "--splash"}, {".dtb", "--dtb"}, {".uname", "--uname"}, {".sbat", "--sbat"}, {".pcrpkey", "--pcrpkey"},
}

// PredictPCR11 runs `systemd-measure calculate` over the UKI's sections and
// returns the expected PCR 11 (sha256) per boot phase.
func PredictPCR11(ctx context.Context, measure, path string) (map[string]string, error) {
	if measure == "" {
		measure = "/usr/lib/systemd/systemd-measure"
	}
	sec, err := Sections(path)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "uki-measure-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	args := []string{"calculate", "--bank=sha256", "--json=short"}
	for _, m := range measured {
		d, ok := sec[m.section]
		if !ok {
			continue
		}
		p := filepath.Join(dir, strings.TrimPrefix(m.section, "."))
		// systemd-stub measures exactly VirtualSize bytes of each section,
		// which is what Sections returns.
		if err := os.WriteFile(p, d, 0o600); err != nil {
			return nil, err
		}
		args = append(args, m.flag+"="+p)
	}
	out := map[string]string{}
	for _, phase := range []string{"enter-initrd", "enter-initrd:leave-initrd", "enter-initrd:leave-initrd:sysinit", "enter-initrd:leave-initrd:sysinit:ready"} {
		b, err := exec.CommandContext(ctx, measure, append(args, "--phase="+phase)...).Output()
		if err != nil {
			return nil, fmt.Errorf("systemd-measure: %w", err)
		}
		var r map[string][]struct {
			PCR  int    `json:"pcr"`
			Hash string `json:"hash"`
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, err
		}
		for _, e := range r["sha256"] {
			if e.PCR == 11 {
				out[phase] = e.Hash
			}
		}
	}
	return out, nil
}
