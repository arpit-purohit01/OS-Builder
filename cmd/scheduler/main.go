// Command scheduler assigns queued builds to build hosts (DRR fair share,
// rendezvous placement, leases, reaper). Run one active instance; a second
// instance is safe (leases are optimistic) but redundant.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"foundry.dev/foundry/internal/pgnotify"
	"foundry.dev/foundry/internal/scheduler"
	"foundry.dev/foundry/internal/store"
	"foundry.dev/foundry/internal/svc"
)

func main() {
	log := svc.Logger("scheduler")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := store.Open(ctx, svc.Env("FOUNDRY_DB", svc.DefaultDSN))
	if err != nil {
		log.Error("db", "err", err)
		os.Exit(1)
	}
	defer db.Close()
	s := scheduler.New(db, log, scheduler.Config{PerOrgMax: svc.EnvInt("FOUNDRY_PER_ORG_MAX", 2)})
	wake := pgnotify.Wake(ctx, db.Pool, "foundry_sched")
	if err := s.Run(ctx, 5*time.Second, wake); err != nil && ctx.Err() == nil {
		log.Error("scheduler", "err", err)
		os.Exit(1)
	}
}
