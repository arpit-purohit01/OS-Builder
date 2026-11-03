package gpt

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewAddWriteRead(t *testing.T) {
	p := filepath.Join(t.TempDir(), "d.img")
	f, _ := os.Create(p)
	defer f.Close()
	const size = 1 << 30
	f.Truncate(size)
	tb, err := New(size, GUIDFrom("disk"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tb.Add(Partition{Type: TypeESP, UUID: GUIDFrom("esp"), FirstLBA: 2048, LastLBA: 2048 + 204800 - 1, Name: "ESP"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteProtectiveMBR(f, size); err != nil {
		t.Fatal(err)
	}
	if err := tb.Write(f); err != nil {
		t.Fatal(err)
	}
	got, err := Read(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Partitions) != 1 || got.Partitions[0].Name != "ESP" || got.DiskGUID() != GUIDFrom("disk") {
		t.Fatalf("%+v", got.Partitions)
	}
	fr := got.FreeRegions()
	if len(fr) != 1 || fr[0].First != 206848 || fr[0].Last != uint64(size/512)-34 {
		t.Fatalf("free regions %+v", fr)
	}
}

func TestFreeRegionsGaps(t *testing.T) {
	tb, _ := New(10<<30, GUID{})
	tb.Add(Partition{Type: TypeESP, UUID: GUIDFrom("a"), FirstLBA: 2048, LastLBA: 206847})
	tb.Add(Partition{Type: TypeMSBasicData, UUID: GUIDFrom("b"), FirstLBA: 4 << 21, LastLBA: (6 << 21) - 1}) // 4–6 GiB
	fr := tb.FreeRegions()
	if len(fr) != 2 || fr[0].First != 6<<21 || fr[1].First != 206848 || fr[1].Last != (4<<21)-1 {
		t.Fatalf("%+v", fr)
	}
}
