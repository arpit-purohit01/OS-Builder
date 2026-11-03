package queue_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foundry.dev/foundry/internal/queue"
	"foundry.dev/foundry/internal/testdb"
)

func TestClaimIsExclusive(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		if err := queue.Enqueue(ctx, db.Pool, queue.Job{Kind: "k"}); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	seen := map[int64]int{}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for {
				j, err := queue.Claim(ctx, db.Pool, []string{"k"}, "w", time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				if j == nil {
					return
				}
				mu.Lock()
				seen[j.ID]++
				mu.Unlock()
				queue.Complete(ctx, db.Pool, j.ID)
			}
		}(w)
	}
	wg.Wait()
	if len(seen) != 50 {
		t.Fatalf("claimed %d distinct jobs", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("job %d claimed %d times", id, n)
		}
	}
}

func TestRetryPermanentRescueUnique(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	queue.Enqueue(ctx, db.Pool, queue.Job{Kind: "r", MaxAttempts: 2})
	j, _ := queue.Claim(ctx, db.Pool, []string{"r"}, "w", time.Minute)
	queue.Fail(ctx, db.Pool, j, errors.New("boom"))
	var state string
	db.Pool.QueryRow(ctx, `SELECT state FROM jobs WHERE id=$1`, j.ID).Scan(&state)
	if state != "available" {
		t.Fatalf("retry state %s", state)
	}
	db.Pool.Exec(ctx, `UPDATE jobs SET run_at=now() WHERE id=$1`, j.ID)
	j, _ = queue.Claim(ctx, db.Pool, []string{"r"}, "w", time.Minute)
	queue.Fail(ctx, db.Pool, j, errors.New("boom"))
	db.Pool.QueryRow(ctx, `SELECT state FROM jobs WHERE id=$1`, j.ID).Scan(&state)
	if state != "failed" {
		t.Fatalf("after max attempts: %s", state)
	}

	queue.Enqueue(ctx, db.Pool, queue.Job{Kind: "p"})
	j, _ = queue.Claim(ctx, db.Pool, []string{"p"}, "w", time.Minute)
	queue.Fail(ctx, db.Pool, j, queue.Permanent{Err: errors.New("bad input")})
	db.Pool.QueryRow(ctx, `SELECT state FROM jobs WHERE id=$1`, j.ID).Scan(&state)
	if state != "failed" {
		t.Fatalf("permanent: %s", state)
	}

	queue.Enqueue(ctx, db.Pool, queue.Job{Kind: "x"})
	j, _ = queue.Claim(ctx, db.Pool, []string{"x"}, "dead-worker", time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	n, err := queue.Rescue(ctx, db.Pool)
	if err != nil || n != 1 {
		t.Fatalf("rescued %d %v", n, err)
	}
	if err := queue.Extend(ctx, db.Pool, j.ID, "dead-worker", time.Minute); err == nil {
		t.Fatal("dead worker kept its lease")
	}

	queue.Enqueue(ctx, db.Pool, queue.Job{Kind: "u", UniqueKey: "build-1"})
	queue.Enqueue(ctx, db.Pool, queue.Job{Kind: "u", UniqueKey: "build-1"})
	var c int
	db.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE kind='u'`).Scan(&c)
	if c != 1 {
		t.Fatalf("unique key not enforced: %d", c)
	}
}

func TestRunner(t *testing.T) {
	db := testdb.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var done atomic.Int32
	r := &queue.Runner{Pool: db.Pool, WorkerID: "t", Concurrency: 3, Poll: 50 * time.Millisecond}
	r.Handle("work", func(ctx context.Context, j *queue.Job) error {
		done.Add(1)
		return nil
	})
	go r.Run(ctx)
	for i := 0; i < 10; i++ {
		queue.Enqueue(context.Background(), db.Pool, queue.Job{Kind: "work"})
	}
	deadline := time.Now().Add(10 * time.Second)
	for done.Load() < 10 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if done.Load() != 10 {
		t.Fatalf("ran %d", done.Load())
	}
}

func TestBackoffBounds(t *testing.T) {
	for a := 0; a < 40; a++ {
		d := queue.Backoff(a, time.Second, time.Minute)
		if d < 0 || d > time.Minute {
			t.Fatalf("attempt %d: %v", a, d)
		}
	}
}
