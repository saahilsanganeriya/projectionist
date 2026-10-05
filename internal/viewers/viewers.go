// Package viewers holds the one rule every disruptive job obeys: pause while anyone but the owner is watching.
// Moving or re-encoding a file someone else is streaming breaks their stream (Plex re-opens the path on every
// range request); the owner's own viewing never blocks a job (his call).
package viewers

import (
	"context"
	"log/slog"
	"time"

	"projectionist/internal/jellyfin"
	"projectionist/internal/plex"
)

// JellyfinActiveWithin matches the renamer's window: a Jellyfin client idle for longer is not watching.
const JellyfinActiveWithin = 120 * time.Second

type Gate struct {
	Plex     *plex.Client
	Jellyfin *jellyfin.Client
	Owners   map[string]bool
	Poll     time.Duration // how often WaitIdle and Watch re-check
	Log      *slog.Logger
}

// Others counts sessions on either server that do not belong to an owner account.
func Others(plexSessions []plex.Session, jfSessions []jellyfin.Session, owners map[string]bool) int {
	n := 0
	for _, s := range plexSessions {
		if !owners[s.User] {
			n++
		}
	}
	for _, s := range jfSessions {
		if s.NowPlayingItem != nil && !owners[s.UserName] {
			n++
		}
	}
	return n
}

// Count asks both servers. An error means "unknown", and callers must treat unknown as busy.
func (g *Gate) Count(ctx context.Context) (int, error) {
	ps, err := g.Plex.Sessions(ctx)
	if err != nil {
		return 0, err
	}
	js, err := g.Jellyfin.Sessions(ctx, JellyfinActiveWithin)
	if err != nil {
		return 0, err
	}
	return Others(ps, js, g.Owners), nil
}

// WaitIdle returns once nobody but an owner is watching, or with ctx's error.
func (g *Gate) WaitIdle(ctx context.Context) error {
	for {
		n, err := g.Count(ctx)
		if err == nil && n == 0 {
			return nil
		}
		g.Log.Info("waiting for viewers", "event", "viewer_wait", "others", n, "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(g.Poll):
		}
	}
}

// Watch calls onChange(busy) whenever the busy state flips (starting from "not busy"), until ctx ends.
// Use it to SIGSTOP/SIGCONT a long-running process; an error reading sessions counts as busy.
func (g *Gate) Watch(ctx context.Context, onChange func(busy bool)) {
	busy := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(g.Poll):
		}
		n, err := g.Count(ctx)
		if now := err != nil || n > 0; now != busy {
			busy = now
			g.Log.Info("viewer state changed", "event", "viewer_change", "busy", busy, "others", n, "err", err)
			onChange(busy)
		}
	}
}
