// Package lbpace is the one Letterboxd rate limit for the whole process: every stage that sends a request to
// letterboxd.com or boxd.it waits on Shared first, so two stages fetching at once still send at most one request
// per Floor between them.
package lbpace

import (
	"context"
	"sync"
	"time"
)

// Floor is the politeness promise: never more than one Letterboxd request per 2 s from this process.
const Floor = 2 * time.Second

// Limiter hands out request start times at least Floor apart, to any number of goroutines.
type Limiter struct {
	mu    sync.Mutex
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
	next  time.Time // the earliest start of the next request
}

func New(now func() time.Time, sleep func(context.Context, time.Duration) error) *Limiter {
	return &Limiter{now: now, sleep: sleep}
}

// Shared is the process-wide limiter.
var Shared = New(time.Now, sleepCtx)

// Wait returns when the caller may send one request. Each caller reserves its own slot before sleeping, so
// concurrent callers queue instead of all waking at once; a cancelled wait still spends its slot.
func (l *Limiter) Wait(ctx context.Context) error {
	l.mu.Lock()
	start := l.now()
	if start.Before(l.next) {
		start = l.next
	}
	l.next = start.Add(Floor)
	l.mu.Unlock()
	if d := start.Sub(l.now()); d > 0 {
		return l.sleep(ctx, d)
	}
	return ctx.Err()
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
