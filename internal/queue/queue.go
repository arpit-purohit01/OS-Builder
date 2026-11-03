// Package queue is a Postgres-backed job queue (ADR-0008). Jobs are claimed
// with FOR UPDATE SKIP LOCKED, leased, retried with full-jitter exponential
// backoff and rescued when a worker dies. This is the only package allowed to
// use SKIP LOCKED.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Job is one unit of work.
type Job struct {
	ID          int64           `json:"id"`
	Kind        string          `json:"kind"`
	OrgID       string          `json:"orgId,omitempty"`
	Payload     json.RawMessage `json:"payload"`
	Priority    int             `json:"priority"`
	Attempt     int             `json:"attempt"`
	MaxAttempts int             `json:"maxAttempts"`
	RunAt       time.Time       `json:"runAt"`
	UniqueKey   string          `json:"uniqueKey,omitempty"`
}

// Execer is satisfied by pools and transactions (enqueue inside a tx).
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Enqueue adds a job. With UniqueKey, a pending job with the same key makes this a no-op.
func Enqueue(ctx context.Context, x Execer, j Job) error {
	if j.MaxAttempts == 0 {
		j.MaxAttempts = 3
	}
	if j.Payload == nil {
		j.Payload = json.RawMessage("{}")
	}
	var runAt any
	if !j.RunAt.IsZero() {
		runAt = j.RunAt
	}
	var uk any
	if j.UniqueKey != "" {
		uk = j.UniqueKey
	}
	_, err := x.Exec(ctx, `INSERT INTO jobs (kind, org_id, payload, priority, max_attempts, run_at, unique_key)
		VALUES ($1, NULLIF($2,''), $3, $4, $5, COALESCE($6, now()), $7) ON CONFLICT DO NOTHING`,
		j.Kind, j.OrgID, []byte(j.Payload), j.Priority, j.MaxAttempts, runAt, uk)
	if err != nil {
		return err
	}
	_, err = x.Exec(ctx, `SELECT pg_notify('foundry_jobs', $1)`, j.Kind)
	return err
}

// Permanent marks an error as not retryable.
type Permanent struct{ Err error }

func (p Permanent) Error() string { return p.Err.Error() }
func (p Permanent) Unwrap() error { return p.Err }

// Backoff is full-jitter exponential backoff: rand(0, min(cap, base·2^attempt)).
func Backoff(attempt int, base, cap time.Duration) time.Duration {
	d := base << uint(min(attempt, 30))
	if d <= 0 || d > cap {
		d = cap
	}
	return time.Duration(rand.Int64N(int64(d) + 1))
}

// Handler processes a job. Returning Permanent{} fails it immediately.
type Handler func(ctx context.Context, j *Job) error

// Runner claims and runs jobs of the registered kinds.
type Runner struct {
	Pool        *pgxpool.Pool
	WorkerID    string
	Concurrency int
	Lease       time.Duration
	Poll        time.Duration
	Log         *slog.Logger
	// OnDead runs when a job fails for the last time (out of attempts or a
	// permanent error), e.g. to fail the build it belongs to.
	OnDead   func(ctx context.Context, j *Job, err error)
	handlers map[string]Handler
}

// Handle registers a handler for a kind.
func (r *Runner) Handle(kind string, h Handler) {
	if r.handlers == nil {
		r.handlers = map[string]Handler{}
	}
	r.handlers[kind] = h
}

// Claim takes one ready job of the given kinds, or returns nil.
func Claim(ctx context.Context, pool *pgxpool.Pool, kinds []string, worker string, lease time.Duration) (*Job, error) {
	j := &Job{}
	var org *string
	err := pool.QueryRow(ctx, `UPDATE jobs SET state='running', locked_by=$2, locked_until=now()+$3::interval, attempt=attempt+1
		WHERE id = (SELECT id FROM jobs WHERE state='available' AND kind = ANY($1) AND run_at <= now()
		            ORDER BY priority DESC, run_at, id FOR UPDATE SKIP LOCKED LIMIT 1)
		RETURNING id, kind, org_id, payload, priority, attempt, max_attempts, run_at, COALESCE(unique_key,'')`,
		kinds, worker, lease.String()).Scan(&j.ID, &j.Kind, &org, &j.Payload, &j.Priority, &j.Attempt, &j.MaxAttempts, &j.RunAt, &j.UniqueKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if org != nil {
		j.OrgID = *org
	}
	return j, err
}

// Complete marks a job done.
func Complete(ctx context.Context, pool *pgxpool.Pool, id int64) error {
	_, err := pool.Exec(ctx, `UPDATE jobs SET state='done', finished_at=now(), locked_by=NULL, locked_until=NULL WHERE id=$1`, id)
	return err
}

// Dead reports whether a failure of j with cause is final.
func Dead(j *Job, cause error) bool {
	var perm Permanent
	return errors.As(cause, &perm) || j.Attempt >= j.MaxAttempts
}

// Fail records an error; retries with backoff unless permanent or out of attempts.
func Fail(ctx context.Context, pool *pgxpool.Pool, j *Job, cause error) error {
	var perm Permanent
	msg := cause.Error()
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	if errors.As(cause, &perm) || j.Attempt >= j.MaxAttempts {
		_, err := pool.Exec(ctx, `UPDATE jobs SET state='failed', last_error=$2, finished_at=now(), locked_by=NULL, locked_until=NULL WHERE id=$1`, j.ID, msg)
		return err
	}
	delay := Backoff(j.Attempt, 2*time.Second, 5*time.Minute)
	_, err := pool.Exec(ctx, `UPDATE jobs SET state='available', last_error=$2, run_at=now()+$3::interval, locked_by=NULL, locked_until=NULL WHERE id=$1`,
		j.ID, msg, delay.String())
	return err
}

// Extend renews a running job's lease.
func Extend(ctx context.Context, pool *pgxpool.Pool, id int64, worker string, lease time.Duration) error {
	ct, err := pool.Exec(ctx, `UPDATE jobs SET locked_until=now()+$3::interval WHERE id=$1 AND locked_by=$2 AND state='running'`, id, worker, lease.String())
	if err == nil && ct.RowsAffected() == 0 {
		return fmt.Errorf("queue: lost lease on job %d", id)
	}
	return err
}

// Rescue returns jobs whose lease expired to the queue (or fails them).
func Rescue(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	ct, err := pool.Exec(ctx, `UPDATE jobs SET
		state = CASE WHEN attempt >= max_attempts THEN 'failed' ELSE 'available' END,
		last_error = COALESCE(last_error,'') || ' [lease expired on ' || COALESCE(locked_by,'?') || ']',
		finished_at = CASE WHEN attempt >= max_attempts THEN now() ELSE NULL END,
		locked_by=NULL, locked_until=NULL
		WHERE state='running' AND locked_until < now()`)
	if err != nil {
		return 0, err
	}
	return ct.RowsAffected(), nil
}

// Run claims and executes jobs until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) error {
	if r.Concurrency <= 0 {
		r.Concurrency = 4
	}
	if r.Lease == 0 {
		r.Lease = 5 * time.Minute
	}
	if r.Poll == 0 {
		r.Poll = time.Second
	}
	if r.Log == nil {
		r.Log = slog.Default()
	}
	kinds := make([]string, 0, len(r.handlers))
	for k := range r.handlers {
		kinds = append(kinds, k)
	}
	wake := make(chan struct{}, 1)
	go r.listen(ctx, wake)
	sem := make(chan struct{}, r.Concurrency)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		select {
		case <-ctx.Done():
			return nil
		case sem <- struct{}{}:
		}
		j, err := Claim(ctx, r.Pool, kinds, r.WorkerID, r.Lease)
		if err != nil || j == nil {
			<-sem
			if err != nil && ctx.Err() == nil {
				r.Log.Error("queue claim", "err", err)
			}
			select {
			case <-ctx.Done():
				return nil
			case <-wake:
			case <-time.After(r.Poll):
			}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			r.exec(ctx, j)
		}()
	}
}

func (r *Runner) exec(ctx context.Context, j *Job) {
	jctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { // lease keep-alive
		t := time.NewTicker(r.Lease / 3)
		defer t.Stop()
		for {
			select {
			case <-jctx.Done():
				return
			case <-t.C:
				if err := Extend(ctx, r.Pool, j.ID, r.WorkerID, r.Lease); err != nil {
					r.Log.Warn("queue lease", "job", j.ID, "err", err)
					cancel()
					return
				}
			}
		}
	}()
	var err error
	func() {
		defer func() {
			if p := recover(); p != nil {
				err = Permanent{fmt.Errorf("panic: %v", p)}
			}
		}()
		err = r.handlers[j.Kind](jctx, j)
	}()
	bg := context.WithoutCancel(ctx)
	if err == nil {
		if cerr := Complete(bg, r.Pool, j.ID); cerr != nil {
			r.Log.Error("queue complete", "job", j.ID, "err", cerr)
		}
		return
	}
	r.Log.Warn("job failed", "job", j.ID, "kind", j.Kind, "attempt", j.Attempt, "err", err)
	if ferr := Fail(bg, r.Pool, j, err); ferr != nil {
		r.Log.Error("queue fail", "job", j.ID, "err", ferr)
	} else if Dead(j, err) && r.OnDead != nil {
		r.OnDead(bg, j, err)
	}
}

func (r *Runner) listen(ctx context.Context, wake chan<- struct{}) {
	for ctx.Err() == nil {
		conn, err := r.Pool.Acquire(ctx)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		_, err = conn.Exec(ctx, "LISTEN foundry_jobs")
		for err == nil {
			_, err = conn.Conn().WaitForNotification(ctx)
			if err == nil {
				select {
				case wake <- struct{}{}:
				default:
				}
			}
		}
		conn.Exec(context.Background(), "UNLISTEN *")
		conn.Release()
	}
}
