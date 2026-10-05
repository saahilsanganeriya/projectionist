package lbpace

import (
	"context"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"
)

// Catches: two stages that each wait on the limiter at the same moment both sending at once (the rate limit kept per
// caller instead of per process), and a third caller not queued behind both. The clock is frozen, so each caller's
// sleep is exactly how far its request start lies past the first.
func TestCallersShareOneSlotEveryTwoSeconds(t *testing.T) {
	t0 := time.Date(2026, 10, 5, 7, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	var slept []time.Duration
	l := New(func() time.Time { return t0 }, func(_ context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		slept = append(slept, d)
		return nil
	})
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Wait(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	slices.Sort(slept)
	if want := []time.Duration{2 * time.Second, 4 * time.Second}; !reflect.DeepEqual(slept, want) {
		t.Errorf("waits = %v, want %v (the first caller goes at once)", slept, want)
	}
}
