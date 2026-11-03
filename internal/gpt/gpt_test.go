package gpt

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGUIDRoundTrip(t *testing.T) {
	s := "e7bb33fb-06cf-4e81-8273-e543b413e2e2"
	if MustParse(s).String() != s {
		t.Fatal(MustParse(s).String())
	}
	if _, err := ParseGUID("nope"); err == nil {
		t.Fatal("bad guid parsed")
	}
	g := GUIDFrom("a", "b")
	if g != GUIDFrom("a", "b") || g == GUIDFrom("a", "c") || g.String()[14] != '4' {
		t.Fatalf("derived guid %s", g)
	}
}

func TestAddPartitionMatchesSfdisk(t *testing.T) {
	if _, err := exec.LookPath("sfdisk"); err != nil {
		t.Skip("sfdisk not installed")
	}
	img := filepath.Join(t.TempDir(), "d.raw")
	f, _ := os.Create(img)
	f.Truncate(64 << 20)
	f.Close()
	layout := "label: gpt\nstart=2048, size=20480, type=" + TypeESP.String() + ", name=esp\n" +
		"start=22528, size=40960, type=" + TypeUsrX86_64.String() + ", name=os_1\n" +
		"start=63488, size=4096, type=" + TypeUsrVerityX86_64.String() + ", name=os_1_v\n" +
		"start=67616, size=40960, type=" + TypeUsrX86_64.String() + ", name=_empty\n"
	cmd := exec.Command("sfdisk", "-q", img)
	cmd.Stdin = strings.NewReader(layout)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sfdisk: %v %s", err, out)
	}
	tb, err := ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}
	if len(tb.Partitions) != 4 {
		t.Fatalf("parts %d", len(tb.Partitions))
	}
	v, ok := tb.ByName("os_1_v")
	if !ok || v.Type != TypeUsrVerityX86_64 || v.FirstLBA != 63488 || v.Size() != 4096*512 {
		t.Fatalf("verity %+v", v)
	}
	gapStart := v.LastLBA + 1 // 67584..67615 is a 32-sector gap
	if !tb.Free(gapStart, gapStart+31) || tb.Free(gapStart, gapStart+32) {
		t.Fatal("free-range check")
	}
	if _, err := tb.Add(Partition{Type: TypeUsrVeritySigX8664, FirstLBA: gapStart, LastLBA: gapStart + 32}); err == nil {
		t.Fatal("overlap accepted")
	}
	p, err := tb.Add(Partition{Type: TypeUsrVeritySigX8664, UUID: GUIDFrom("x"), FirstLBA: gapStart, LastLBA: gapStart + 31, Attrs: 1 << 60, Name: "os_1_s"})
	if err != nil {
		t.Fatal(err)
	}
	f, _ = os.OpenFile(img, os.O_RDWR, 0)
	if err := tb.Write(f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command("sfdisk", "--verify", img).CombinedOutput()
	if err != nil || strings.Contains(string(out), "corrupt") {
		t.Fatalf("verify: %v %s", err, out)
	}
	dump, _ := exec.Command("sfdisk", "-d", img).Output()
	if !strings.Contains(string(dump), `name="os_1_s"`) || !strings.Contains(strings.ToUpper(string(dump)), strings.ToUpper(TypeUsrVeritySigX8664.String())) {
		t.Fatalf("dump: %s", dump)
	}
	// backup header must be valid: corrupt the primary and let sfdisk read the backup.
	tb2, err := ReadFile(img)
	if err != nil || len(tb2.Partitions) != 5 || tb2.Partitions[len(tb2.Partitions)-1].Index != p.Index {
		t.Fatalf("reread: %v %+v", err, tb2)
	}
	f, _ = os.OpenFile(img, os.O_RDWR, 0)
	f.WriteAt(make([]byte, 512), 512)
	f.Close()
	dump, _ = exec.Command("sfdisk", "-d", img).CombinedOutput()
	if !strings.Contains(string(dump), `name="os_1_s"`) {
		t.Fatalf("backup GPT not usable: %s", dump)
	}
}
