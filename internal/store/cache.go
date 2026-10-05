package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// The http_cache table holds raw API responses (TMDB, fanart.tv) so stages asking the same question on the same
// day cost one request. Keys carry the client's prefix ("tmdb:", "fanart:") and never a secret.

// CacheGet returns the body stored under key if it was fetched at or after since.
func (s *Store) CacheGet(ctx context.Context, key string, since time.Time) (body []byte, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT body FROM http_cache WHERE key=? AND fetched_at>=?`, key, ts(since)).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	return body, err == nil, err
}

// CachePut stores body under key, replacing any older copy.
func (s *Store) CachePut(ctx context.Context, key string, body []byte, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO http_cache(key, body, fetched_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET body=excluded.body, fetched_at=excluded.fetched_at`, key, body, ts(at))
	return err
}

// CachePrune deletes the entries under prefix fetched before before, which bounds the table to one TTL's worth.
func (s *Store) CachePrune(ctx context.Context, prefix string, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM http_cache WHERE substr(key, 1, ?)=? AND fetched_at<?`, len(prefix), prefix, ts(before))
	return err
}
