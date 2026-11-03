package builder

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	for in, bad := range map[string]string{
		"-o Acquire::http::Proxy=http://bld_1:3f9a2c@10.0.2.100:3128 update": "3f9a2c",
		"Authorization: Bearer fdy_pat_abc":                                  "fdy_pat_abc",
		"user ops hash $6$salt$abcdef":                                       "abcdef",
		"-----BEGIN PRIVATE KEY----- MIIE":                                   "MIIE",
	} {
		if out := Redact(in); strings.Contains(out, bad) {
			t.Errorf("%q → %q still contains %q", in, out, bad)
		}
	}
	if Redact("Get:1 http://archive.ubuntu.com/ubuntu noble InRelease") != "Get:1 http://archive.ubuntu.com/ubuntu noble InRelease" {
		t.Fatal("benign line changed")
	}
}
