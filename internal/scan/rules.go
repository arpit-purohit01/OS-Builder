package scan

import (
	"sort"
	"strings"
)

// Matcher is an Aho–Corasick automaton over lowercase bytes: one pass finds
// every occurrence of every pattern in O(n + matches).
type Matcher struct {
	next []map[byte]int
	fail []int
	out  [][]int // pattern indexes ending at state
	pats []string
}

// NewMatcher builds the automaton.
func NewMatcher(patterns []string) *Matcher {
	m := &Matcher{next: []map[byte]int{{}}, fail: []int{0}, out: [][]int{nil}}
	for i, p := range patterns {
		p = strings.ToLower(p)
		m.pats = append(m.pats, p)
		s := 0
		for j := 0; j < len(p); j++ {
			c := p[j]
			n, ok := m.next[s][c]
			if !ok {
				n = len(m.next)
				m.next = append(m.next, map[byte]int{})
				m.fail = append(m.fail, 0)
				m.out = append(m.out, nil)
				m.next[s][c] = n
			}
			s = n
		}
		m.out[s] = append(m.out[s], i)
	}
	// BFS for failure links.
	queue := []int{}
	for _, n := range m.next[0] {
		queue = append(queue, n)
	}
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		for c, n := range m.next[s] {
			queue = append(queue, n)
			f := m.fail[s]
			for {
				if t, ok := m.next[f][c]; ok && t != n {
					m.fail[n] = t
					break
				}
				if f == 0 {
					m.fail[n] = 0
					break
				}
				f = m.fail[f]
			}
			m.out[n] = append(m.out[n], m.out[m.fail[n]]...)
		}
	}
	return m
}

// Find returns the set of pattern indexes occurring in text.
func (m *Matcher) Find(text []byte) map[int]bool {
	found := map[int]bool{}
	s := 0
	for _, b := range text {
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		for {
			if n, ok := m.next[s][b]; ok {
				s = n
				break
			}
			if s == 0 {
				break
			}
			s = m.fail[s]
		}
		for _, i := range m.out[s] {
			found[i] = true
		}
	}
	return found
}

// ScriptRule fires when every group has at least one matching token.
type ScriptRule struct {
	ID       string
	Feature  string // weight key, or "" with Severity blocker
	Severity string
	Groups   [][]string
	Detail   string
}

// ScriptRules are the v1 content rules for scripts and text files in
// overlays (red-team corpus: test/redteam).
var ScriptRules = []ScriptRule{
	{ID: "SCR-001", Feature: "download-exec", Severity: SevHigh, Detail: "downloads and executes code",
		Groups: [][]string{{"curl ", "wget ", "fetch "}, {"| sh", "|sh", "| bash", "|bash", "bash -c", "sh -c", "| python", "|python", "&& chmod +x", ";chmod +x", "; chmod +x"}}},
	{ID: "SCR-002", Feature: "obfuscation", Severity: SevHigh, Detail: "decodes and executes an encoded payload",
		Groups: [][]string{{"base64 -d", "base64 --decode", "xxd -r", "openssl enc -d", "b64decode"}, {"| sh", "|sh", "| bash", "|bash", "eval", "exec(", "bash -c", "sh -c"}}},
	{ID: "SCR-003", Severity: SevBlocker, Detail: "reverse shell",
		Groups: [][]string{{"/dev/tcp/", "/dev/udp/", "nc -e", "ncat -e", "socat exec:", "socat tcp", "mkfifo /tmp/", "bash -i >&"}}},
	{ID: "SCR-004", Feature: "critical-prefix", Severity: SevCritical, Detail: "writes SSH authorized keys",
		Groups: [][]string{{"authorized_keys"}}},
	{ID: "SCR-005", Feature: "critical-prefix", Severity: SevCritical, Detail: "preloads libraries into every process",
		Groups: [][]string{{"ld.so.preload", "ld_preload="}}},
	{ID: "SCR-006", Feature: "critical-prefix", Severity: SevCritical, Detail: "modifies sudo configuration",
		Groups: [][]string{{"/etc/sudoers"}}},
	{ID: "SCR-007", Feature: "security-disabled", Severity: SevMedium, Detail: "disables a security control",
		Groups: [][]string{{"aa-teardown", "systemctl disable apparmor", "systemctl stop apparmor", "ufw disable", "nft flush ruleset", "iptables -f", "setenforce 0", "systemctl mask apparmor", "apparmor=0"}}},
	{ID: "SCR-008", Feature: "unit-not-catalog", Severity: SevMedium, Detail: "installs a scheduled job",
		Groups: [][]string{{"/etc/cron", "crontab ", "/var/spool/cron"}}},
	{ID: "SCR-009", Feature: "obfuscation", Severity: SevHigh, Detail: "makes files immutable (persistence)",
		Groups: [][]string{{"chattr +i", "chattr +a"}}},
	{ID: "MAL-001", Severity: SevBlocker, Detail: "EICAR test signature", Groups: [][]string{{"x5o!p%@ap[4\\pzx54(p^)7cc)7}$eicar-standard-antivirus-test-file!$h+h*"}}},
	// YARA-style signatures (malware families and tooling seen in the wild).
	{ID: "YAR-MINER", Severity: SevBlocker, Detail: "cryptocurrency miner", Groups: [][]string{{"stratum+tcp://", "stratum+ssl://", "xmrig", "cryptonight", "minexmr", "nicehash", "--donate-level"}}},
	{ID: "YAR-WEBSHELL", Severity: SevBlocker, Detail: "web shell", Groups: [][]string{{"c99shell", "r57shell", "b374k", "wso shell", "<?php eval($_post", "<?php system($_get", "<?php @eval("}}},
	{ID: "YAR-ROOTKIT", Severity: SevBlocker, Detail: "rootkit or process hider", Groups: [][]string{{"libprocesshider", "diamorphine", "reptile_module", "/proc/self/hide", "hide_pid", "kovid"}}},
	{ID: "YAR-BOTNET", Severity: SevBlocker, Detail: "botnet client", Groups: [][]string{{"/bin/busybox mirai", "mirai", "gafgyt", "bashlite", "tsunami ddos"}, {"attack", "flood", "scanner", "killer"}}},
	{ID: "YAR-CREDSTEAL", Severity: SevHigh, Detail: "credential harvesting", Groups: [][]string{{"/etc/shadow", "id_rsa", "wallet.dat", ".aws/credentials", "login data"}, {"curl ", "wget ", "nc ", "/dev/tcp/", "http://", "https://"}}},
}

var scriptMatcher, scriptIndex = buildScriptMatcher()

func buildScriptMatcher() (*Matcher, [][2]int) {
	var pats []string
	var idx [][2]int // pattern → (rule, group)
	for ri, r := range ScriptRules {
		for gi, g := range r.Groups {
			for _, tok := range g {
				pats = append(pats, tok)
				idx = append(idx, [2]int{ri, gi})
			}
		}
	}
	return NewMatcher(pats), idx
}

// ScanText applies the script rules to one file's content.
func ScanText(path string, content []byte) []Finding {
	hits := scriptMatcher.Find(content)
	groupHit := map[[2]int]bool{}
	for p := range hits {
		groupHit[scriptIndex[p]] = true
	}
	var out []Finding
	for ri, r := range ScriptRules {
		all := true
		for gi := range r.Groups {
			if !groupHit[[2]int{ri, gi}] {
				all = false
				break
			}
		}
		if all {
			w := Weights[r.Feature]
			scanner := "script"
			if strings.HasPrefix(r.ID, "MAL-") || strings.HasPrefix(r.ID, "YAR-") {
				scanner = "yara"
			}
			out = append(out, Finding{Rule: r.ID, Feature: r.Feature, Severity: r.Severity, Weight: w, Path: path, Detail: r.Detail, Scanner: scanner})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rule < out[j].Rule })
	return out
}

// ScanContent applies script and signature rules to a file's content.
func ScanContent(path string, content []byte) []Finding { return ScanText(path, content) }
