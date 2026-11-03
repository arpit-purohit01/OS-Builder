package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"foundry.dev/foundry/internal/store"
	"foundry.dev/foundry/internal/testdb"
)

func setup(t *testing.T) (*store.DB, *store.Org, *store.User, *store.Project) {
	db := testdb.New(t)
	ctx := context.Background()
	o, err := db.CreateOrg(ctx, "Acme", "acme", "pilot")
	if err != nil {
		t.Fatal(err)
	}
	u, err := db.UpsertUser(ctx, "ada@example.com", "Ada")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddMember(ctx, o.ID, u.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"c", "c1"} {
		if err := db.EnsureCatalogVersion(ctx, c, []byte{1}, false); err != nil {
			t.Fatal(err)
		}
	}
	p, err := db.CreateProject(ctx, o.ID, "Aurora", "aurora")
	if err != nil {
		t.Fatal(err)
	}
	return db, o, u, p
}

func TestMigrateIdempotent(t *testing.T) {
	db := testdb.New(t)
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestTenancyIsolation(t *testing.T) {
	db, o, _, p := setup(t)
	ctx := context.Background()
	other, _ := db.CreateOrg(ctx, "Other", "other", "free")
	if _, err := db.GetProject(ctx, other.ID, p.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-tenant read must be not found, got %v", err)
	}
	if _, err := db.GetProject(ctx, o.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateProject(ctx, o.ID, "dup", "aurora"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
}

func TestSpecVersionIdempotentAndPaging(t *testing.T) {
	db, o, u, p := setup(t)
	ctx := context.Background()
	mk := func(d string) *store.SpecVersion {
		return &store.SpecVersion{OrgID: o.ID, ProjectID: p.ID, Digest: d, APIVersion: "foundry.dev/v1alpha1", Spec: json.RawMessage(`{}`), CatalogVersion: "c1", CreatedBy: u.ID}
	}
	d1 := "sha256:" + "11" + string(make([]byte, 0)) + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	a, created, err := db.CreateSpecVersion(ctx, mk(d1))
	if err != nil || !created {
		t.Fatal(err, created)
	}
	b, created, err := db.CreateSpecVersion(ctx, mk(d1))
	if err != nil || created || b.ID != a.ID {
		t.Fatalf("expected idempotent insert: %v %v %s %s", err, created, a.ID, b.ID)
	}
	for i := 0; i < 5; i++ {
		d := "sha256:" + string(rune('a'+i)) + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		if _, _, err := db.CreateSpecVersion(ctx, mk(d)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	var all []store.SpecVersion
	var cur *store.Cursor
	for {
		page, next, err := db.ListSpecVersions(ctx, o.ID, p.ID, cur, 2)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page...)
		if next == nil {
			break
		}
		cur = next
	}
	if len(all) != 6 {
		t.Fatalf("paging returned %d", len(all))
	}
	seen := map[string]bool{}
	for _, v := range all {
		if seen[v.ID] {
			t.Fatal("duplicate across pages")
		}
		seen[v.ID] = true
	}
}

func TestBuildStateMachine(t *testing.T) {
	db, o, u, p := setup(t)
	ctx := context.Background()
	sv, _, _ := db.CreateSpecVersion(ctx, &store.SpecVersion{OrgID: o.ID, ProjectID: p.ID, Digest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", APIVersion: "v", Spec: json.RawMessage(`{}`), CatalogVersion: "c", CreatedBy: u.ID})
	b, err := db.CreateBuild(ctx, &store.Build{OrgID: o.ID, ProjectID: p.ID, SpecVersionID: sv.ID, Arch: "x86_64", CatalogVersion: "c", CreatedBy: u.ID, PredictedMinutes: 20})
	if err != nil {
		t.Fatal(err)
	}
	if b.State != store.StateQueued {
		t.Fatal(b.State)
	}
	if err := db.Transition(ctx, b.ID, store.StateQueued, store.StateBuilt, store.TransitionOpts{}); !errors.Is(err, store.ErrIllegalTransition) {
		t.Fatalf("expected illegal transition, got %v", err)
	}
	if err := db.Lease(ctx, b.ID, "host-1", time.Minute); err != nil {
		t.Fatal(err)
	}
	// optimistic check: second lease must fail
	if err := db.Lease(ctx, b.ID, "host-2", time.Minute); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("double lease: %v", err)
	}
	if err := db.Transition(ctx, b.ID, store.StateLeased, store.StateBuilding, store.TransitionOpts{Actor: "host-1"}); err != nil {
		t.Fatal(err)
	}
	if cancel, err := db.RenewLease(ctx, b.ID, "host-1", time.Minute); err != nil || cancel {
		t.Fatal(err, cancel)
	}
	if _, err := db.RenewLease(ctx, b.ID, "host-2", time.Minute); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign host renew: %v", err)
	}
	// lease expiry → requeue with attempt+1
	db.Pool.Exec(ctx, `UPDATE builds SET lease_expires_at=now()-interval '1s' WHERE id=$1`, b.ID)
	exp, _ := db.ExpiredLeases(ctx)
	if len(exp) != 1 {
		t.Fatalf("expired %d", len(exp))
	}
	if err := db.Transition(ctx, b.ID, store.StateBuilding, store.StateQueued, store.TransitionOpts{Actor: "reaper"}); err != nil {
		t.Fatal(err)
	}
	b2, _ := db.GetBuild(ctx, o.ID, b.ID)
	if b2.Attempt != 1 || b2.LeaseExpiresAt != nil {
		t.Fatalf("attempt %d lease %v", b2.Attempt, b2.LeaseExpiresAt)
	}
	ev, _ := db.BuildEvents(ctx, o.ID, b.ID)
	if len(ev) != 4 || ev[3].To != "queued" {
		t.Fatalf("events %+v", ev)
	}
	// cancel while queued
	c, err := db.RequestCancel(ctx, o.ID, b.ID)
	if err != nil || c.State != store.StateCancelled {
		t.Fatal(err, c.State)
	}
}

func TestLogsAndArtifacts(t *testing.T) {
	db, o, u, p := setup(t)
	ctx := context.Background()
	sv, _, _ := db.CreateSpecVersion(ctx, &store.SpecVersion{OrgID: o.ID, ProjectID: p.ID, Digest: "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", APIVersion: "v", Spec: json.RawMessage(`{}`), CatalogVersion: "c", CreatedBy: u.ID})
	b, _ := db.CreateBuild(ctx, &store.Build{OrgID: o.ID, ProjectID: p.ID, SpecVersionID: sv.ID, Arch: "x86_64", CatalogVersion: "c", CreatedBy: u.ID})
	db.AppendLogs(ctx, b.ID, "build", []string{"a", "b"})
	db.AppendLogs(ctx, b.ID, "build", []string{"c"})
	ls, err := db.Logs(ctx, o.ID, b.ID, 1, 10)
	if err != nil || len(ls) != 2 || ls[0].Line != "b" || ls[1].Seq != 3 {
		t.Fatalf("logs %+v %v", ls, err)
	}
	if err := db.PutArtifact(ctx, o.ID, store.Artifact{BuildID: b.ID, Kind: "qcow2", Digest: "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", Size: 42}); err != nil {
		t.Fatal(err)
	}
	as, _ := db.Artifacts(ctx, o.ID, b.ID)
	if len(as) != 1 || as[0].Size != 42 {
		t.Fatalf("%+v", as)
	}
}

func TestSessionsTokensCodes(t *testing.T) {
	db, o, u, _ := setup(t)
	ctx := context.Background()
	s, secret, err := db.CreateSession(ctx, u.ID, "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.SessionByToken(ctx, secret)
	if err != nil || got.ID != s.ID {
		t.Fatal(err)
	}
	if _, err := db.SessionByToken(ctx, secret+"x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("bad token accepted")
	}
	tok, sec, err := db.CreateToken(ctx, o.ID, u.ID, "ci", []string{"builds:write"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if tt, err := db.TokenBySecret(ctx, sec); err != nil || tt.ID != tok.ID {
		t.Fatal(err)
	}
	db.RevokeToken(ctx, o.ID, tok.ID)
	if _, err := db.TokenBySecret(ctx, sec); err == nil {
		t.Fatal("revoked token accepted")
	}
	db.CreateLoginCode(ctx, "ada@example.com", "123456", time.Minute)
	for i := 0; i < 5; i++ {
		ok, _ := db.ConsumeLoginCode(ctx, "ada@example.com", "000000")
		if ok {
			t.Fatal("wrong code accepted")
		}
	}
	if ok, _ := db.ConsumeLoginCode(ctx, "ada@example.com", "123456"); ok {
		t.Fatal("code accepted after 5 failed attempts")
	}
	db.CreateLoginCode(ctx, "ada@example.com", "654321", time.Minute)
	if ok, err := db.ConsumeLoginCode(ctx, "ada@example.com", "654321"); !ok || err != nil {
		t.Fatal("valid code rejected", err)
	}
	if ok, _ := db.ConsumeLoginCode(ctx, "ada@example.com", "654321"); ok {
		t.Fatal("code reused")
	}
}

func TestAuditChain(t *testing.T) {
	db, o, u, _ := setup(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := db.Audit(ctx, o.ID, u.ID, "test.action", "x", map[string]int{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
	if bad, err := db.VerifyAuditChain(ctx); err != nil || bad != 0 {
		t.Fatalf("chain broken at %d: %v", bad, err)
	}
	db.Pool.Exec(ctx, `UPDATE audit_log SET action='tampered' WHERE id = (SELECT min(id)+2 FROM audit_log)`)
	if bad, _ := db.VerifyAuditChain(ctx); bad == 0 {
		t.Fatal("tampering not detected")
	}
}

func TestCompareReproduced(t *testing.T) {
	db, o, u, p := setup(t)
	ctx := context.Background()
	sv, _, _ := db.CreateSpecVersion(ctx, &store.SpecVersion{OrgID: o.ID, ProjectID: p.ID, Digest: "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", APIVersion: "v", Spec: json.RawMessage(`{}`), CatalogVersion: "c", CreatedBy: u.ID})
	mk := func(tree, hash string) string {
		b, err := db.CreateBuild(ctx, &store.Build{OrgID: o.ID, ProjectID: p.ID, SpecVersionID: sv.ID, Arch: "x86_64", CatalogVersion: "c", TreeDigest: tree, CreatedBy: u.ID, PredictedMinutes: 20})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.SetBuildResult(ctx, b.ID, strings.Repeat(hash, 64), ""); err != nil {
			t.Fatal(err)
		}
		return b.ID
	}
	tree := "sha256:" + strings.Repeat("a", 64)
	a := mk(tree, "1")
	if _, ok, err := db.CompareReproduced(ctx, a); err != nil || ok != nil {
		t.Fatalf("first build has nothing to compare: %v %v", ok, err)
	}
	b := mk(tree, "1")
	if other, ok, err := db.CompareReproduced(ctx, b); err != nil || ok == nil || !*ok || other != a {
		t.Fatalf("matching builds: %s %v %v", other, ok, err)
	}
	c := mk(tree, "2")
	if _, ok, _ := db.CompareReproduced(ctx, c); ok == nil || *ok {
		t.Fatal("different usr hash must not reproduce")
	}
	other := mk("sha256:"+strings.Repeat("b", 64), "3")
	if _, ok, _ := db.CompareReproduced(ctx, other); ok != nil {
		t.Fatal("different trees are not comparable")
	}
	got, _ := db.GetBuildAny(ctx, a)
	if got.Reproduced == nil {
		t.Fatal("verdict not stored")
	}
}
