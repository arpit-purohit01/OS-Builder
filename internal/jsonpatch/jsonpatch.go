// Package jsonpatch implements RFC 6902 JSON Patch (apply), a minimal diff
// producing add/remove/replace operations, inverse patches for undo, and a
// three-way merge for spec remixes (algorithms §16).
package jsonpatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// Op is one patch operation.
type Op struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	From  string `json:"from,omitempty"`
	Value any    `json:"value,omitempty"`
}

// Patch is a list of operations.
type Patch []Op

var ErrTest = errors.New("jsonpatch: test failed")

// ParsePointer splits an RFC 6901 pointer.
func ParsePointer(p string) ([]string, error) {
	if p == "" {
		return nil, nil
	}
	if !strings.HasPrefix(p, "/") {
		return nil, fmt.Errorf("jsonpatch: invalid pointer %q", p)
	}
	parts := strings.Split(p[1:], "/")
	for i, s := range parts {
		parts[i] = strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
	}
	return parts, nil
}

// EscapeToken escapes one pointer token.
func EscapeToken(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

func clone(v any) any {
	b, _ := json.Marshal(v)
	var out any
	json.Unmarshal(b, &out)
	return out
}

// Apply applies p to doc (decoded JSON) and returns a new document.
func Apply(doc any, p Patch) (any, error) {
	doc = clone(doc)
	for i, op := range p {
		var err error
		switch op.Op {
		case "add":
			doc, err = set(doc, op.Path, clone(op.Value), true)
		case "remove":
			doc, _, err = remove(doc, op.Path)
		case "replace":
			if _, err = get(doc, op.Path); err == nil {
				doc, err = set(doc, op.Path, clone(op.Value), false)
			}
		case "move":
			var v any
			if strings.HasPrefix(op.Path+"/", op.From+"/") && op.Path != op.From {
				err = fmt.Errorf("jsonpatch: cannot move %s into itself", op.From)
				break
			}
			if doc, v, err = remove(doc, op.From); err == nil {
				doc, err = set(doc, op.Path, v, true)
			}
		case "copy":
			var v any
			if v, err = get(doc, op.From); err == nil {
				doc, err = set(doc, op.Path, clone(v), true)
			}
		case "test":
			var v any
			if v, err = get(doc, op.Path); err == nil && !equal(v, op.Value) {
				err = ErrTest
			}
		default:
			err = fmt.Errorf("jsonpatch: unknown op %q", op.Op)
		}
		if err != nil {
			return nil, fmt.Errorf("op %d (%s %s): %w", i, op.Op, op.Path, err)
		}
	}
	return doc, nil
}

func equal(a, b any) bool { return reflect.DeepEqual(clone(a), clone(b)) }

func get(doc any, path string) (any, error) {
	parts, err := ParsePointer(path)
	if err != nil {
		return nil, err
	}
	cur := doc
	for _, p := range parts {
		switch c := cur.(type) {
		case map[string]any:
			v, ok := c[p]
			if !ok {
				return nil, fmt.Errorf("jsonpatch: %s not found", path)
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(p)
			if err != nil || i < 0 || i >= len(c) {
				return nil, fmt.Errorf("jsonpatch: bad index in %s", path)
			}
			cur = c[i]
		default:
			return nil, fmt.Errorf("jsonpatch: %s not found", path)
		}
	}
	return cur, nil
}

// set writes value at path. insert=true uses "add" semantics for arrays.
func set(doc any, path string, value any, insert bool) (any, error) {
	parts, err := ParsePointer(path)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return value, nil
	}
	parent, err := get(doc, joinPointer(parts[:len(parts)-1]))
	if err != nil {
		return nil, err
	}
	last := parts[len(parts)-1]
	switch c := parent.(type) {
	case map[string]any:
		c[last] = value
		return doc, nil
	case []any:
		var i int
		if last == "-" {
			i = len(c)
		} else if i, err = strconv.Atoi(last); err != nil || i < 0 || i > len(c) || (!insert && i == len(c)) {
			return nil, fmt.Errorf("jsonpatch: bad index %q", last)
		}
		var n []any
		if insert {
			n = append(append(append([]any{}, c[:i]...), value), c[i:]...)
		} else {
			n = append([]any{}, c...)
			n[i] = value
		}
		return set(doc, joinPointer(parts[:len(parts)-1]), n, false)
	default:
		return nil, fmt.Errorf("jsonpatch: parent of %s is not a container", path)
	}
}

func remove(doc any, path string) (any, any, error) {
	parts, err := ParsePointer(path)
	if err != nil || len(parts) == 0 {
		return nil, nil, fmt.Errorf("jsonpatch: cannot remove %q", path)
	}
	parent, err := get(doc, joinPointer(parts[:len(parts)-1]))
	if err != nil {
		return nil, nil, err
	}
	last := parts[len(parts)-1]
	switch c := parent.(type) {
	case map[string]any:
		v, ok := c[last]
		if !ok {
			return nil, nil, fmt.Errorf("jsonpatch: %s not found", path)
		}
		delete(c, last)
		return doc, v, nil
	case []any:
		i, err := strconv.Atoi(last)
		if err != nil || i < 0 || i >= len(c) {
			return nil, nil, fmt.Errorf("jsonpatch: bad index %q", last)
		}
		v := c[i]
		n := append(append([]any{}, c[:i]...), c[i+1:]...)
		d, err := set(doc, joinPointer(parts[:len(parts)-1]), n, false)
		return d, v, err
	default:
		return nil, nil, fmt.Errorf("jsonpatch: %s not found", path)
	}
}

func joinPointer(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString("/" + EscapeToken(p))
	}
	return b.String()
}

// Diff returns a patch transforming a into b. Arrays are replaced whole
// (spec arrays are small sets); objects are diffed key by key.
func Diff(a, b any) Patch {
	var p Patch
	diff("", clone(a), clone(b), &p)
	return p
}

func diff(path string, a, b any, p *Patch) {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		keys := map[string]bool{}
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			sub := path + "/" + EscapeToken(k)
			av, ina := am[k]
			bv, inb := bm[k]
			switch {
			case ina && !inb:
				*p = append(*p, Op{Op: "remove", Path: sub})
			case !ina && inb:
				*p = append(*p, Op{Op: "add", Path: sub, Value: bv})
			default:
				diff(sub, av, bv, p)
			}
		}
		return
	}
	if !reflect.DeepEqual(a, b) {
		*p = append(*p, Op{Op: "replace", Path: path, Value: b})
	}
}

// Inverse computes the patch that undoes p when applied to Apply(doc, p).
func Inverse(doc any, p Patch) (Patch, error) {
	after, err := Apply(doc, p)
	if err != nil {
		return nil, err
	}
	return Diff(after, doc), nil
}

// Conflict describes overlapping edits in a three-way merge.
type Conflict struct {
	Path string `json:"path"`
	Ours any    `json:"ours"`
	Up   any    `json:"upstream"`
}

// Merge3 rebases ours (a fork of base) onto upstream. Edits to disjoint
// paths merge; the same path edited differently is a conflict (ours kept).
// Arrays of strings are merged as sets: union of additions minus removals.
func Merge3(base, ours, upstream any) (any, []Conflict) {
	var conflicts []Conflict
	out := merge3("", clone(base), clone(ours), clone(upstream), &conflicts)
	return out, conflicts
}

func merge3(path string, base, ours, up any, cs *[]Conflict) any {
	if reflect.DeepEqual(ours, up) {
		return ours
	}
	if reflect.DeepEqual(base, ours) {
		return up
	}
	if reflect.DeepEqual(base, up) {
		return ours
	}
	bm, b1 := base.(map[string]any)
	om, o1 := ours.(map[string]any)
	um, u1 := up.(map[string]any)
	if o1 && u1 {
		if !b1 {
			bm = map[string]any{}
		}
		res := map[string]any{}
		keys := map[string]bool{}
		for _, m := range []map[string]any{bm, om, um} {
			for k := range m {
				keys[k] = true
			}
		}
		for k := range keys {
			bv, ob := bm[k]
			ov, oo := om[k]
			uv, ou := um[k]
			switch {
			case oo && ou:
				res[k] = merge3(path+"/"+EscapeToken(k), bv, ov, uv, cs)
			case oo && !ou:
				if ob && reflect.DeepEqual(bv, ov) {
					continue // upstream deleted, we didn't touch
				}
				res[k] = ov
			case !oo && ou:
				if ob && reflect.DeepEqual(bv, uv) {
					continue // we deleted, upstream didn't touch
				}
				res[k] = uv
			}
		}
		return res
	}
	if sb, ok := stringSet(base); ok {
		if so, ok := stringSet(ours); ok {
			if su, ok := stringSet(up); ok {
				return setMerge(sb, so, su)
			}
		}
	}
	*cs = append(*cs, Conflict{Path: path, Ours: ours, Up: up})
	return ours
}

func stringSet(v any) ([]string, bool) {
	if v == nil {
		return []string{}, true
	}
	a, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(a))
	for _, x := range a {
		s, ok := x.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

func setMerge(base, ours, up []string) []any {
	in := func(xs []string, s string) bool {
		for _, x := range xs {
			if x == s {
				return true
			}
		}
		return false
	}
	var out []any
	seen := map[string]bool{}
	for _, s := range append(append([]string{}, ours...), up...) {
		if seen[s] {
			continue
		}
		removedByOurs := in(base, s) && !in(ours, s)
		removedByUp := in(base, s) && !in(up, s)
		if removedByOurs || removedByUp {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
