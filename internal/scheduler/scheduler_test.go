package scheduler

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"foundry.dev/foundry/internal/store"
	"foundry.dev/foundry/internal/testdb"
)

func TestFairShareCapsAndPlacement(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	a := testdb.NewSeed(t, db, "aaa")
	b := testdb.NewSeed(t, db, "bbb")
	// org a floods the queue first; org b submits one build later.
	var aBuilds []*store.Build
	for i := 0; i < 6; i++ {
		aBuilds = append(aBuilds, a.Build(t, 0, 10))
	}
	bb := b.Build(t, 0, 10)
	for _, h := range []string{"h1", "h2"} {
		if err := db.UpsertHost(ctx, store.Host{ID: h, Pool: "build", Arch: "x86_64", Slots: 2}); err != nil {
			t.Fatal(err)
		}
	}
	db.UpsertHost(ctx, store.Host{ID: "arm", Pool: "build", Arch: "aarch64", Slots: 4})
	s := New(db, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{PerOrgMax: 2})
	res, err := s.Tick(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 4 x86 slots, but org a is capped at 2: a gets 2, b gets 1.
	if len(res.Assigned) != 3 {
		t.Fatalf("assigned %v", res.Assigned)
	}
	if _, ok := res.Assigned[bb.ID]; !ok {
		t.Fatal("org b starved behind org a's backlog")
	}
	for id, h := range res.Assigned {
		if h == "arm" {
			t.Fatalf("x86 build %s placed on arm host", id)
		}
	}
	perOrg, _, _ := db.RunningCount(ctx)
	if perOrg[a.Org.ID] != 2 || perOrg[b.Org.ID] != 1 {
		t.Fatalf("running %v", perOrg)
	}
	// Leased builds are visible to their host.
	got := 0
	for _, h := range []string{"h1", "h2"} {
		ls, _ := db.LeasedTo(ctx, h)
		got += len(ls)
	}
	if got != 3 {
		t.Fatalf("leased to hosts: %d", got)
	}
	// A cancelled queued build leaves the DRR on the next tick.
	if _, err := db.RequestCancel(ctx, a.Org.ID, aBuilds[5].ID); err != nil {
		t.Fatal(err)
	}
	res, _ = s.Tick(ctx)
	if res.Queued != 3 || len(res.Assigned) != 0 {
		t.Fatalf("after cancel: queued=%d assigned=%v", res.Queued, res.Assigned)
	}
	// Stale hosts get nothing.
	db.Pool.Exec(ctx, `UPDATE hosts SET last_seen = now() - interval '10 minutes'`)
	db.Pool.Exec(ctx, `UPDATE builds SET state='built' WHERE state='leased'`) // free the slots (test shortcut)
	if res, _ := s.Tick(ctx); len(res.Assigned) != 0 {
		t.Fatalf("assigned to stale host: %v", res.Assigned)
	}
}

func TestReaperRequeuesThenFails(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	sd := testdb.NewSeed(t, db, "rrr")
	bld := sd.Build(t, 0, 5)
	db.UpsertHost(ctx, store.Host{ID: "h1", Pool: "build", Arch: "x86_64", Slots: 1})
	s := New(db, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{MaxAttempts: 2, LeaseTTL: time.Minute})
	expire := func() {
		db.Pool.Exec(ctx, `UPDATE builds SET lease_expires_at = now() - interval '1 second' WHERE id=$1`, bld.ID)
	}
	for round := 0; round < 2; round++ {
		res, err := s.Tick(ctx)
		if err != nil || res.Assigned[bld.ID] != "h1" {
			t.Fatalf("round %d: %v %v", round, err, res)
		}
		expire()
	}
	res, err := s.Tick(ctx)
	if err != nil || res.Reaped != 1 {
		t.Fatalf("reap: %v %+v", err, res)
	}
	got, _ := db.GetBuildAny(ctx, bld.ID)
	if got.State != store.StateFailed || got.Attempt != 1 {
		t.Fatalf("after max attempts: %s attempt=%d failure=%s", got.State, got.Attempt, got.Failure)
	}
}
