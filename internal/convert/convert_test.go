package convert

import (
	"archive/tar"
	"context"
	"encoding/xml"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"foundry.dev/foundry/internal/imgutil"
)

func TestConvertAll(t *testing.T) {
	for _, x := range []string{"qemu-img", "xz"} {
		if _, err := exec.LookPath(x); err != nil {
			t.Skip(x + " missing")
		}
	}
	dir := t.TempDir()
	disk := filepath.Join(dir, "os_1.0.raw")
	f, _ := os.Create(disk)
	f.Truncate(64 << 20)
	f.WriteAt([]byte("EFI PART data"), 512)
	f.Close()
	out, err := Run(context.Background(), Options{Compression: imgutil.Compression{XzLevel: 1}}, disk, dir, []string{"img.xz", "qcow2", "ova"})
	if err != nil {
		t.Fatal(err)
	}
	if out["vmdk"] != "" {
		t.Fatal("vmdk kept although only ova was requested")
	}
	for _, k := range []string{"img.xz", "qcow2", "ova"} {
		if _, err := os.Stat(out[k]); err != nil {
			t.Fatalf("%s: %v", k, err)
		}
	}
	info, _ := exec.Command("qemu-img", "info", out["qcow2"]).Output()
	if !strings.Contains(string(info), "virtual size: 64 MiB") {
		t.Fatalf("qcow2: %s", info)
	}
	// xz round trip
	back, _ := exec.Command("xz", "-dc", out["img.xz"]).Output()
	if len(back) != 64<<20 || string(back[512:525]) != "EFI PART data" {
		t.Fatal("img.xz content")
	}
	// OVA: ovf first, manifest, then vmdk; OVF is well-formed XML.
	of, _ := os.Open(out["ova"])
	defer of.Close()
	tr := tar.NewReader(of)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		names = append(names, h.Name)
		if strings.HasSuffix(h.Name, ".ovf") {
			b, _ := io.ReadAll(tr)
			var v struct{ XMLName xml.Name }
			if err := xml.Unmarshal(b, &v); err != nil || v.XMLName.Local != "Envelope" {
				t.Fatalf("ovf xml: %v", err)
			}
			if !strings.Contains(string(b), `vmw:value="efi"`) {
				t.Fatal("ovf without EFI firmware")
			}
		}
	}
	if len(names) != 3 || !strings.HasSuffix(names[0], ".ovf") || !strings.HasSuffix(names[1], ".mf") || !strings.HasSuffix(names[2], ".vmdk") {
		t.Fatalf("ova order: %v", names)
	}
	if !Valid("qcow2") || Valid("exe") {
		t.Fatal("Valid")
	}
	if d := Default(true, false); len(d) != 1 || d[0] != "qcow2" {
		t.Fatalf("default %v", d)
	}
}
