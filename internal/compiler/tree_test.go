package compiler

import (
	"archive/tar"
	"bytes"
	"strings"
	"testing"
)

func TestTreeTarRoundTrip(t *testing.T) {
	tr := newTree()
	tr.add("mkosi.conf", 0o644, "[Output]\n")
	tr.add("mkosi.extra/usr/bin/x", 0o755, "#!/bin/sh\n")
	tr.asset("mkosi.extra/usr/share/wall.png", "sha256:"+strings.Repeat("a", 64))
	tr.component("mkosi.extra/usr/lib/foundry/foundry-agent", "foundry-agent", 0o755)
	tr.symlink("mkosi.extra/etc/l", "../usr/bin/x")
	var buf bytes.Buffer
	if err := tr.WriteTar(&buf); err != nil {
		t.Fatal(err)
	}
	back, err := ReadTar(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if back.Digest() != tr.Digest() {
		t.Fatal("digest changed across tar round trip")
	}
	if f := back.Files["mkosi.extra/usr/share/wall.png"]; f.AssetRef != tr.Files["mkosi.extra/usr/share/wall.png"].AssetRef {
		t.Fatalf("asset ref lost: %+v", f)
	}
	if back.Files["mkosi.extra/usr/bin/x"].Mode != 0o755 {
		t.Fatal("mode lost")
	}
	// A tar with an escaping path is rejected.
	var evil bytes.Buffer
	tw := tar.NewWriter(&evil)
	tw.WriteHeader(&tar.Header{Name: "../etc/passwd", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
	tw.Write([]byte("x"))
	tw.Close()
	if _, err := ReadTar(&evil); err == nil {
		t.Fatal("path traversal accepted")
	}
}
