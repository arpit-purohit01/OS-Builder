package scan

import (
	"sort"
)

// Severity levels (matches the findings table).
const (
	SevInfo     = "info"
	SevLow      = "low"
	SevMedium   = "medium"
	SevHigh     = "high"
	SevCritical = "critical"
	SevBlocker  = "blocker"
)

// Finding is one scanner observation.
type Finding struct {
	Rule     string `json:"rule"`
	Feature  string `json:"feature,omitempty"` // weight key (caps apply per feature)
	Severity string `json:"severity"`
	Weight   int    `json:"weight"`
	Path     string `json:"path,omitempty"`
	Detail   string `json:"detail"`
	Scanner  string `json:"scanner"` // sbom|cve|malware|yara|script|diff|behavior
}

// Feature weights (security-threat-model.md §6, v1).
var Weights = map[string]int{
	"critical-prefix":    40,
	"setuid":             25,
	"modified-binary":    30,
	"unit-not-catalog":   15,
	"download-exec":      30,
	"obfuscation":        25,
	"dns-unknown":        20,
	"listen-not-catalog": 15,
	"security-disabled":  20,
	"critical-cve":       10,
	"uploaded-elf":       10,
	"new-creator":        10,
	"expert-script":      5,
	"expert-repo":        15,
	"expert-cmdline":     10,
	"expert-overlay":     5,
}

// caps per rule (e.g. DNS 20 each, max 40).
var caps = map[string]int{"dns-unknown": 40, "critical-cve": 30}

// Decision is the outcome of scoring.
type Decision struct {
	Score       int       `json:"score"`
	Blocked     bool      `json:"blocked"`
	Reviewers   int       `json:"reviewers"` // humans required: 0 (auto), 1, 2
	Tier        string    `json:"tier"`      // verified | reviewed (after approval) | private-unverified
	Explanation []string  `json:"explanation"`
	Findings    []Finding `json:"findings"`
}

// Score combines findings into a decision. expert reports whether the spec
// uses expert.* content; reproducible whether a double build matched (nil =
// not run).
func Score(findings []Finding, expert bool, reproducible *bool) Decision {
	d := Decision{Findings: findings}
	perRule := map[string]int{}
	for _, f := range findings {
		if f.Severity == SevBlocker {
			d.Blocked = true
			d.Explanation = append(d.Explanation, "blocked: "+f.Rule+": "+f.Detail)
			continue
		}
		key := f.Feature
		if key == "" {
			key = f.Rule
		}
		w := f.Weight
		if c, ok := caps[key]; ok && perRule[key]+w > c {
			w = max(0, c-perRule[key])
		}
		perRule[key] += w
		d.Score += w
	}
	if d.Score > 100 {
		d.Score = 100
	}
	rules := make([]string, 0, len(perRule))
	for r := range perRule {
		rules = append(rules, r)
	}
	sort.Strings(rules)
	for _, r := range rules {
		if perRule[r] > 0 {
			d.Explanation = append(d.Explanation, r+": +"+itoa(perRule[r]))
		}
	}
	switch {
	case d.Blocked:
		d.Reviewers, d.Tier = 0, ""
	case d.Score > 60:
		d.Reviewers, d.Tier = 2, "reviewed"
	case d.Score >= 20 || expert:
		d.Reviewers, d.Tier = 1, "reviewed"
	default:
		d.Reviewers, d.Tier = 0, "verified"
		if reproducible != nil && !*reproducible {
			// Catalog-only but not reproducible: still auto-approved, but not "verified".
			d.Tier = "private-unverified"
			d.Explanation = append(d.Explanation, "double build did not reproduce: tier private-unverified")
		}
	}
	return d
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
