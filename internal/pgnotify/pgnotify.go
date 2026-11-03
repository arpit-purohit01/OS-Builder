// Package pgnotify delivers Postgres NOTIFY payloads to a callback, holding
// one pooled connection and reconnecting with backoff.
package pgnotify

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Listen blocks until ctx ends, calling f for every notification on channel.
func Listen(ctx context.Context, pool *pgxpool.Pool, channel string, f func(payload string)) {
	backoff := 100 * time.Millisecond
	for ctx.Err() == nil {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			sleep(ctx, &backoff)
			continue
		}
		_, err = conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize())
		for err == nil {
			var n *pgconnNotification
			n, err = wait(ctx, conn)
			if err == nil {
				backoff = 100 * time.Millisecond
				f(n.Payload)
			}
		}
		conn.Exec(context.Background(), "UNLISTEN *")
		conn.Release()
		if ctx.Err() == nil {
			sleep(ctx, &backoff)
		}
	}
}

type pgconnNotification struct{ Payload string }

func wait(ctx context.Context, conn *pgxpool.Conn) (*pgconnNotification, error) {
	n, err := conn.Conn().WaitForNotification(ctx)
	if err != nil {
		return nil, err
	}
	return &pgconnNotification{Payload: n.Payload}, nil
}

func sleep(ctx context.Context, d *time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(*d):
	}
	if *d < 10*time.Second {
		*d *= 2
	}
}

// Wake returns a channel that receives (coalesced) signals for channel.
func Wake(ctx context.Context, pool *pgxpool.Pool, channel string) <-chan struct{} {
	c := make(chan struct{}, 1)
	go Listen(ctx, pool, channel, func(string) {
		select {
		case c <- struct{}{}:
		default:
		}
	})
	return c
}
