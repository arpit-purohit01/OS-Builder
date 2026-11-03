package jsonpatch

import (
	"encoding/json"
	"math/rand"
	"reflect"
	"testing"
)

func doc(s string) any {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		panic(err)
	}
	return v
}

func TestApplyRFCOps(t *testing.T) {
	d := doc(`{"a":{"b":[1,2,3]},"c":"x"}`)
	p := Patch{
		{Op: "add", Path: "/a/b/1", Value: 9},
		{Op: "remove", Path: "/a/b/0"},
		{Op: "replace", Path: "/c", Value: "y"},
		{Op: "copy", From: "/c", Path: "/d"},
		{Op: "move", From: "/d", Path: "/a/e"},
		{Op: "add", Path: "/a/b/-", Value: 4},
		{Op: "test", Path: "/a/e", Value: "y"},
		{Op: "add", Path: "/k~1x", Value: true},
	}
	got, err := Apply(d, p)
	if err != nil {
		t.Fatal(err)
	}
	want := doc(`{"a":{"b":[9,2,3,4],"e":"y"},"c":"y","k/x":true}`)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if !reflect.DeepEqual(d, doc(`{"a":{"b":[1,2,3]},"c":"x"}`)) {
		t.Fatal("input mutated")
	}
	for _, bad := range []Patch{
		{{Op: "replace", Path: "/nope", Value: 1}},
		{{Op: "remove", Path: "/a/b/7"}},
		{{Op: "test", Path: "/c", Value: "z"}},
		{{Op: "move", From: "/a", Path: "/a/x"}},
		{{Op: "bogus", Path: "/c"}},
	} {
		if _, err := Apply(d, bad); err == nil {
			t.Fatalf("expected error for %+v", bad)
		}
	}
}

func randDoc(r *rand.Rand, depth int) any {
	if depth == 0 || r.Intn(3) == 0 {
		switch r.Intn(4) {
		case 0:
			return float64(r.Intn(5))
		case 1:
			return []any{"a", "b"}[:r.Intn(3)]
		case 2:
			return r.Intn(2) == 0
		default:
			return []string{"x", "y", "z"}[r.Intn(3)]
		}
	}
	m := map[string]any{}
	for i := 0; i < r.Intn(4); i++ {
		m[string(rune('a'+r.Intn(5)))] = randDoc(r, depth-1)
	}
	return m
}

func TestDiffApplyRoundTripAndInverse(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	for i := 0; i < 2000; i++ {
		a := clone(map[string]any{"root": randDoc(r, 3)})
		b := clone(map[string]any{"root": randDoc(r, 3)})
		p := Diff(a, b)
		got, err := Apply(a, p)
		if err != nil {
			t.Fatalf("apply diff: %v (%v → %v, patch %v)", err, a, b, p)
		}
		if !reflect.DeepEqual(got, b) {
			t.Fatalf("diff round trip: %v != %v", got, b)
		}
		inv, err := Inverse(a, p)
		if err != nil {
			t.Fatal(err)
		}
		back, _ := Apply(got, inv)
		if !reflect.DeepEqual(back, a) {
			t.Fatal("inverse failed")
		}
	}
}

func TestMerge3(t *testing.T) {
	base := doc(`{"look":{"wallpaper":"a","accent":"#111111"},"apps":{"system":["cups"]}}`)
	ours := doc(`{"look":{"wallpaper":"forest","accent":"#111111"},"apps":{"system":["cups","wireguard"]}}`)
	up := doc(`{"look":{"wallpaper":"a","accent":"#222222"},"apps":{"system":["cups","ssh-client"]},"kernel":{"flavor":"hardened"}}`)
	got, cs := Merge3(base, ours, up)
	if len(cs) != 0 {
		t.Fatalf("conflicts %v", cs)
	}
	want := doc(`{"look":{"wallpaper":"forest","accent":"#222222"},"apps":{"system":["cups","wireguard","ssh-client"]},"kernel":{"flavor":"hardened"}}`)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	// both changed the same scalar differently → conflict, ours kept
	up2 := doc(`{"look":{"wallpaper":"dusk","accent":"#111111"},"apps":{"system":["cups"]}}`)
	got, cs = Merge3(base, ours, up2)
	if len(cs) != 1 || cs[0].Path != "/look/wallpaper" {
		t.Fatalf("conflicts %v", cs)
	}
	if got.(map[string]any)["look"].(map[string]any)["wallpaper"] != "forest" {
		t.Fatal("ours not kept")
	}
	// upstream removed an app we did not touch → removed
	up3 := doc(`{"look":{"wallpaper":"a","accent":"#111111"},"apps":{"system":[]}}`)
	got, _ = Merge3(base, ours, up3)
	if !reflect.DeepEqual(got.(map[string]any)["apps"], doc(`{"system":["wireguard"]}`)) {
		t.Fatalf("set merge %v", got)
	}
}
