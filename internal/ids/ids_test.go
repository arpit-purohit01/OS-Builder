package ids

import (
	"sort"
	"testing"
	"time"
)

func TestSortableAndValid(t *testing.T) {
	var xs []string
	for i := 0; i < 50; i++ {
		xs = append(xs, New("bld"))
		time.Sleep(time.Millisecond)
	}
	if !sort.StringsAreSorted(xs) {
		t.Fatal("ids not time-sortable")
	}
	for _, x := range xs {
		if !Valid(x, "bld") {
			t.Fatalf("invalid %s", x)
		}
	}
	if Valid("bld_xyz", "bld") || Valid(New("org"), "bld") {
		t.Fatal("validation too lax")
	}
	if len(Secret(20)) != 32 {
		t.Fatalf("secret len %d", len(Secret(20)))
	}
}
