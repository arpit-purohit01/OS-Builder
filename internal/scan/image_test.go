package scan

import "testing"

func TestBaselineSnapshot(t *testing.T) {
	db, err := ParsePkgDB([]byte(
		"11111111111111111111111111111111  /usr/bin/tool\tcoreutils\n" +
			"22222222222222222222222222222222  /etc/pam.d/common-auth\tlibpam-runtime\n" +
			"33333333333333333333333333333333  /etc/pam.d/common-auth\t@base\n" + // pam-auth-update rewrote it before the snapshot
			"44444444444444444444444444444444  /usr/lib/foo/helper\t@base\n" +
			"@link\t/usr/lib/systemd/system/sockets.target.wants/dbus.socket\t../dbus.socket\n"))
	if err != nil {
		t.Fatal(err)
	}
	m := Manifest{
		{Path: "/usr/bin/tool", Type: "f", MD5: "11111111111111111111111111111111"},
		{Path: "/etc/pam.d/common-auth", Type: "f", MD5: "33333333333333333333333333333333"},
		{Path: "/usr/lib/foo/helper", Type: "f", MD5: "44444444444444444444444444444444"},
		{Path: "/usr/lib/systemd/system/sockets.target.wants/dbus.socket", Type: "l", Link: "../dbus.socket"},
	}
	f, sum := AnalyzeImage(ImageInput{Manifest: m, PkgDB: db})
	if len(f) != 0 {
		t.Fatalf("platform output flagged: %+v", f)
	}
	if sum.Generated != 2 || sum.Owned != 2 {
		t.Fatalf("summary %+v", sum)
	}
	// After the snapshot: a changed baseline file, a retargeted link and a new unit.
	m[1].MD5 = "99999999999999999999999999999999"
	m[3].Link = "/tmp/evil.socket"
	m = append(m, FileEntry{Path: "/usr/lib/systemd/system/backdoor.service", Type: "f", MD5: "55555555555555555555555555555555", SHA256: "x"})
	f, _ = AnalyzeImage(ImageInput{Manifest: m, PkgDB: db})
	got := map[string]string{}
	for _, x := range f {
		got[x.Path] = x.Rule
	}
	for p, r := range map[string]string{
		"/etc/pam.d/common-auth":                                   "DIFF-MODIFIED",
		"/usr/lib/systemd/system/sockets.target.wants/dbus.socket": "DIFF-MODIFIED",
		"/usr/lib/systemd/system/backdoor.service":                 "DIFF-UNOWNED",
	} {
		if got[p] != r {
			t.Errorf("%s: want %s, findings %+v", p, r, got)
		}
	}
}

func TestStockUnitLinks(t *testing.T) {
	db, _ := ParsePkgDB([]byte(
		"11111111111111111111111111111111  /usr/lib/systemd/system/systemd-networkd.service\tsystemd\n" +
			"22222222222222222222222222222222  /usr/lib/systemd/system/ssh.service\topenssh-server\n" +
			"33333333333333333333333333333333  /usr/lib/systemd/system/custom.service\tvendor-agent\n" +
			"44444444444444444444444444444444  /lib/systemd/system/getty@.service\tsystemd\n"))
	m := Manifest{
		{Path: "/etc/systemd/system/multi-user.target.wants/systemd-networkd.service", Type: "l", Link: "/usr/lib/systemd/system/systemd-networkd.service"},
		{Path: "/etc/systemd/system/getty.target.wants/getty@tty1.service", Type: "l", Link: "/lib/systemd/system/getty@.service"},
		{Path: "/etc/systemd/system/ctrl-alt-del.target", Type: "l", Link: "/dev/null"},
		{Path: "/etc/systemd/system/multi-user.target.wants/ssh.service", Type: "l", Link: "/usr/lib/systemd/system/ssh.service"},
		{Path: "/etc/systemd/system/multi-user.target.wants/custom.service", Type: "l", Link: "/usr/lib/systemd/system/custom.service"},
		{Path: "/etc/systemd/system/multi-user.target.wants/backdoor.service", Type: "l", Link: "/opt/backdoor/backdoor.service"},
		{Path: "/etc/locale.conf", Type: "f", MD5: "55555555555555555555555555555555"},
	}
	f, _ := AnalyzeImage(ImageInput{Manifest: m, PkgDB: db, CustomerPkgs: map[string]bool{"vendor-agent": true}})
	got := map[string]bool{}
	for _, x := range f {
		got[x.Path] = true
	}
	for _, p := range []string{"/etc/systemd/system/multi-user.target.wants/ssh.service", "/etc/systemd/system/multi-user.target.wants/custom.service", "/etc/systemd/system/multi-user.target.wants/backdoor.service"} {
		if !got[p] {
			t.Errorf("not flagged: %s", p)
		}
	}
	if len(got) != 3 {
		t.Errorf("benign platform links flagged: %v", got)
	}
}
