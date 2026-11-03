package upload

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type ent struct {
	name, link string
	typ        byte
	mode       int64
	data       string
	size       int64
}

func mkTar(t testing.TB, gz bool, es ...ent) []byte {
	var buf bytes.Buffer
	var w *tar.Writer
	var g *gzip.Writer
	if gz {
		g = gzip.NewWriter(&buf)
		w = tar.NewWriter(g)
	} else {
		w = tar.NewWriter(&buf)
	}
	for _, e := range es {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		size := int64(len(e.data))
		if typ != tar.TypeReg {
			size = 0
		}
		w.WriteHeader(&tar.Header{Name: e.name, Linkname: e.link, Typeflag: typ, Mode: mode, Size: size, Format: tar.FormatPAX})
		if typ == tar.TypeReg {
			w.Write([]byte(e.data))
		}
	}
	w.Close()
	if g != nil {
		g.Close()
	}
	return buf.Bytes()
}

func rules(r *Report) string {
	var s []string
	for _, f := range r.Findings {
		s = append(s, f.Rule)
	}
	return strings.Join(s, ",")
}

func TestArchiveAttacks(t *testing.T) {
	cases := map[string]struct {
		e    ent
		rule string
	}{
		"absolute":       {ent{name: "/etc/shadow", data: "x"}, "UPL-PATH"},
		"dotdot":         {ent{name: "usr/../../etc/passwd", data: "x"}, "UPL-PATH"},
		"symlink escape": {ent{name: "usr/share/x", typ: tar.TypeSymlink, link: "../../../../etc/shadow"}, "UPL-LINK"},
		"symlink abs":    {ent{name: "usr/share/y", typ: tar.TypeSymlink, link: "/etc/shadow"}, "UPL-LINK"},
		"hardlink":       {ent{name: "usr/a", typ: tar.TypeLink, link: "usr/b"}, "UPL-HARDLINK"},
		"device":         {ent{name: "dev/sda", typ: tar.TypeBlock}, "UPL-TYPE"},
		"fifo":           {ent{name: "tmp/p", typ: tar.TypeFifo}, "UPL-TYPE"},
		"setuid":         {ent{name: "usr/bin/x", mode: 0o4755, data: "\x7fELF"}, "UPL-SUID"},
		"control chars":  {ent{name: "usr/a\x01b", data: "x"}, "UPL-PATH"},
	}
	for name, c := range cases {
		for _, gz := range []bool{false, true} {
			rep, err := ValidateArchive(bytes.NewReader(mkTar(t, gz, c.e)), DefaultLimits)
			if err != nil {
				t.Fatal(err)
			}
			if rep.Valid || !strings.Contains(rules(rep), c.rule) {
				t.Errorf("%s (gz=%v): want %s, got valid=%v %s", name, gz, c.rule, rep.Valid, rules(rep))
			}
		}
	}
}

func TestArchiveBenign(t *testing.T) {
	data := mkTar(t, true,
		ent{name: "usr/share/foundry/", typ: tar.TypeDir, mode: 0o755},
		ent{name: "usr/share/foundry/hello.txt", data: "hi"},
		ent{name: "usr/share/foundry/run.sh", data: "#!/bin/sh\necho hi\n", mode: 0o755},
		ent{name: "usr/share/foundry/link", typ: tar.TypeSymlink, link: "hello.txt"},
	)
	rep, err := ValidateArchive(bytes.NewReader(data), DefaultLimits)
	if err != nil || !rep.Valid {
		t.Fatalf("%v %+v", err, rep)
	}
	if len(rep.Scripts) != 1 || rep.Entries != 4 {
		t.Fatalf("%+v", rep)
	}
}

func TestArchiveBombAndLimits(t *testing.T) {
	big := strings.Repeat("\x00", 40<<20)
	rep, _ := ValidateArchive(bytes.NewReader(mkTar(t, true, ent{name: "zeros", data: big})), DefaultLimits)
	if rep.Valid || !strings.Contains(rules(rep), "UPL-RATIO") {
		t.Fatalf("bomb not detected: %s", rules(rep))
	}
	lim := DefaultLimits
	lim.MaxFileBytes = 10
	rep, _ = ValidateArchive(bytes.NewReader(mkTar(t, false, ent{name: "f", data: strings.Repeat("a", 100)})), lim)
	if rep.Valid {
		t.Fatal("oversize file accepted")
	}
	lim = DefaultLimits
	lim.MaxEntries = 2
	rep, _ = ValidateArchive(bytes.NewReader(mkTar(t, false, ent{name: "a", data: "1"}, ent{name: "b", data: "1"}, ent{name: "c", data: "1"})), lim)
	if rep.Valid {
		t.Fatal("entry limit not enforced")
	}
}

func TestImages(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 64, 32))
	img.Set(1, 1, color.White)
	var b bytes.Buffer
	png.Encode(&b, img)
	if r := ValidateImage(b.Bytes(), 64*32); !r.Valid || r.Format != "png" || r.Width != 64 {
		t.Fatalf("%+v", r)
	}
	if r := ValidateImage(b.Bytes(), 100); r.Valid {
		t.Fatal("too many pixels accepted")
	}
	if r := ValidateImage(b.Bytes()[:50], 1<<20); r.Valid {
		t.Fatal("truncated png accepted")
	}
	if r := ValidateImage([]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), 1<<20); r.Valid {
		t.Fatal("svg script accepted")
	}
	if r := ValidateImage([]byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect width="1" height="1"/></svg>`), 1<<20); !r.Valid {
		t.Fatalf("clean svg rejected %+v", r)
	}
	if r := ValidateImage([]byte("MZ\x90\x00"), 1<<20); r.Valid {
		t.Fatal("exe accepted as image")
	}
}

func FuzzArchive(f *testing.F) {
	f.Add(mkTar(f, false, ent{name: "a", data: "b"}))
	f.Add(mkTar(f, true, ent{name: "../x", data: "b"}))
	f.Fuzz(func(t *testing.T, data []byte) {
		lim := DefaultLimits
		lim.MaxTotalBytes = 1 << 20
		ValidateArchive(bytes.NewReader(data), lim) // must not panic
	})
}

func TestMkosiDropinAllowlist(t *testing.T) {
	ok := ValidateMkosiDropin([]byte("# extra\n[Content]\nPackages=htop\n         curl\nRemoveFiles=/usr/share/doc\n"))
	if !ok.Valid {
		t.Fatalf("%+v", ok.Findings)
	}
	for _, bad := range []string{"[Validation]\nSecureBoot=no\n", "[Content]\nKernelCommandLine=init=/bin/sh\n", "[Output]\nOutputDirectory=/tmp\n",
		"[Content]\nExtraTrees=/etc:/etc\n", "[Build]\nToolsTree=/\n", "[Content]\nBootable=no\n"} {
		if ValidateMkosiDropin([]byte(bad)).Valid {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestValidateDebAndKey(t *testing.T) {
	for _, x := range []string{"dpkg-deb", "gpg"} {
		if _, err := exec.LookPath(x); err != nil {
			t.Skip(x + " missing")
		}
	}
	ctx := context.Background()
	dir := t.TempDir()
	pkg := filepath.Join(dir, "pkg")
	os.MkdirAll(filepath.Join(pkg, "DEBIAN"), 0o755)
	os.MkdirAll(filepath.Join(pkg, "usr/share/hello"), 0o755)
	os.WriteFile(filepath.Join(pkg, "usr/share/hello/x"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(pkg, "DEBIAN/control"), []byte("Package: acme-hello\nVersion: 1.0\nArchitecture: all\nMaintainer: a <a@b>\nDescription: test\n"), 0o644)
	os.WriteFile(filepath.Join(pkg, "DEBIAN/postinst"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	deb := filepath.Join(dir, "x.deb")
	if out, err := exec.Command("dpkg-deb", "--root-owner-group", "--build", pkg, deb).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	r := ValidateDeb(ctx, deb)
	if !r.Valid || r.Package != "acme-hello" || len(r.Scripts) == 0 {
		t.Fatalf("deb: %+v", r)
	}
	if ValidateDeb(ctx, filepath.Join(pkg, "DEBIAN/control")).Valid {
		t.Fatal("non-deb accepted")
	}
	home := filepath.Join(dir, "gnupg")
	os.MkdirAll(home, 0o700)
	if out, err := exec.Command("gpg", "--homedir", home, "--batch", "--passphrase", "", "--quick-gen-key", "Repo <repo@example.com>", "ed25519", "sign", "1y").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	pub := filepath.Join(dir, "key.asc")
	out, _ := exec.Command("gpg", "--homedir", home, "--armor", "--export").Output()
	os.WriteFile(pub, out, 0o644)
	k := ValidateGPGKey(ctx, pub)
	if !k.Valid || len(k.Fingerprint) != 40 {
		t.Fatalf("key: %+v", k)
	}
	sec := filepath.Join(dir, "sec.asc")
	out, _ = exec.Command("gpg", "--homedir", home, "--batch", "--pinentry-mode", "loopback", "--passphrase", "", "--armor", "--export-secret-keys").Output()
	os.WriteFile(sec, out, 0o644)
	if ValidateGPGKey(ctx, sec).Valid {
		t.Fatal("secret key accepted")
	}
}
