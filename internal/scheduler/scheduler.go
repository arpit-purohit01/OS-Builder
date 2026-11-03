// Package scheduler assigns queued builds to build hosts: deficit round
// robin across orgs (fair share by predicted minutes), a per-org concurrency
// cap, weighted rendezvous hashing for placement (cache affinity: retries of
// a build land on the same host), leases with renewal, and a reaper that
// re-queues builds whose host stopped renewing.
package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"foundry.dev/foundry/internal/sched"
	"foundry.dev/foundry/internal/store"
)

// Config tunes the scheduler.
type Config struct {
	LeaseTTL    time.Duration // default 2m; workers renew every TTL/4
	HostMaxAge  time.Duration // heartbeat freshness, default 45s
	PerOrgMax   int           // concurrent builds per org, default 2
	Quantum     float64       // DRR quantum in predicted minutes, default 20
	MaxAttempts int           // lease losses before failing, default 3
}

// Scheduler is long-lived: DRR deficits persist across ticks.
type Scheduler struct {
	DB  *store.DB
	Log *slog.Logger
	Cfg Config

	drr  *sched.DRR
	arch map[string]string // build id → arch
	org  map[string]string // build id → org
}

// New creates a scheduler.
func New(db *store.DB, log *slog.Logger, cfg Config) *Scheduler {
	if cfg.LeaseTTL == 0 {
		cfg.LeaseTTL = 2 * time.Minute
	}
	if cfg.HostMaxAge == 0 {
		cfg.HostMaxAge = 45 * time.Second
	}
	if cfg.PerOrgMax == 0 {
		cfg.PerOrgMax = 2
	}
	if cfg.Quantum == 0 {
		cfg.Quantum = 20
	}
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = 3
	}
	return &Scheduler{DB: db, Log: log, Cfg: cfg, drr: sched.NewDRR(cfg.Quantum, nil), arch: map[string]string{}, org: map[string]string{}}
}

// TickResult summarizes one scheduling round.
type TickResult struct {
	Reaped   int
	Assigned map[string]string // build → host
	Queued   int
}

// Tick runs one round: reap expired leases, sync the queue, assign.
func (s *Scheduler) Tick(ctx context.Context) (*TickResult, error) {
	res := &TickResult{Assigned: map[string]string{}}
	n, err := s.reap(ctx)
	if err != nil {
		return res, err
	}
	res.Reaped = n
	queued, err := s.DB.QueuedBuilds(ctx, 1000)
	if err != nil {
		return res, err
	}
	seen := map[string]bool{}
	for _, b := range queued {
		seen[b.ID] = true
		if !s.drr.Has(b.ID) {
			cost := b.PredictedMinutes
			if cost <= 0 {
				cost = 20
			}
			s.drr.Push(sched.Item{ID: b.ID, Org: b.OrgID, Cost: cost, Priority: b.Priority, Seq: b.CreatedAt.UnixNano()})
			s.arch[b.ID], s.org[b.ID] = b.Arch, b.OrgID
		}
	}
	for id := range s.arch {
		if !seen[id] {
			s.drr.Remove(id)
			delete(s.arch, id)
			delete(s.org, id)
		}
	}
	res.Queued = s.drr.Len()
	hosts, err := s.DB.LiveHosts(ctx, "build", s.Cfg.HostMaxAge)
	if err != nil {
		return res, err
	}
	perOrg, perHost, err := s.DB.RunningCount(ctx)
	if err != nil {
		return res, err
	}
	free := map[string]int{}
	total := 0
	for _, h := range hosts {
		f := h.Slots - perHost[h.ID]
		if f > 0 {
			free[h.ID] = f
			total += f
		}
	}
	for total > 0 {
		it, ok := s.drr.Next(func(it sched.Item) bool {
			return perOrg[it.Org] < s.Cfg.PerOrgMax && len(candidates(hosts, free, s.arch[it.ID])) > 0
		})
		if !ok {
			break
		}
		h, ok := sched.Rendezvous(it.ID, candidates(hosts, free, s.arch[it.ID]))
		if !ok {
			break
		}
		err := s.DB.Lease(ctx, it.ID, h.ID, s.Cfg.LeaseTTL)
		delete(s.arch, it.ID)
		delete(s.org, it.ID)
		if errors.Is(err, store.ErrConflict) {
			continue // cancelled or taken meanwhile
		}
		if err != nil {
			return res, err
		}
		free[h.ID]--
		total--
		perOrg[it.Org]++
		res.Assigned[it.ID] = h.ID
		s.DB.Notify(ctx, "foundry_lease", h.ID)
		s.Log.Info("leased", "build", it.ID, "org", it.Org, "host", h.ID)
	}
	return res, nil
}

func candidates(hosts []store.Host, free map[string]int, arch string) []sched.Host {
	var out []sched.Host
	for _, h := range hosts {
		if free[h.ID] > 0 && (arch == "" || h.Arch == arch) {
			out = append(out, sched.Host{ID: h.ID, Weight: float64(h.Slots) * h.Weight, Free: free[h.ID]})
		}
	}
	return out
}

// reap re-queues (or fails) builds whose lease expired.
func (s *Scheduler) reap(ctx context.Context) (int, error) {
	exp, err := s.DB.ExpiredLeases(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, b := range exp {
		host := ""
		if b.HostID != nil {
			host = *b.HostID
		}
		if b.Attempt+1 >= s.Cfg.MaxAttempts {
			err = s.DB.Transition(ctx, b.ID, b.State, store.StateFailed, store.TransitionOpts{Actor: "scheduler",
				Failure: map[string]any{"stage": "lease", "reason": "build host stopped responding " + strconv.Itoa(s.Cfg.MaxAttempts) + " times", "retryable": true}})
		} else {
			err = s.DB.Transition(ctx, b.ID, b.State, store.StateQueued, store.TransitionOpts{Actor: "scheduler",
				Detail: map[string]string{"reason": "lease expired", "host": host}})
		}
		if errors.Is(err, store.ErrConflict) {
			continue
		}
		if err != nil {
			return n, err
		}
		n++
		s.Log.Warn("lease expired", "build", b.ID, "host", host, "attempt", b.Attempt)
	}
	return n, nil
}

// Run ticks until ctx ends; wake (e.g. from LISTEN) triggers an early tick.
func (s *Scheduler) Run(ctx context.Context, every time.Duration, wake <-chan struct{}) error {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			s.Log.Error("scheduler tick", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		case <-wake:
		}
	}
}
