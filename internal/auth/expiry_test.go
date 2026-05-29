package auth

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestComputeExpiry covers the sliding-window / hard-cap math without touching
// the database. The Service value is constructed bare (no pool) since
// computeExpiry doesn't query anything.
func TestComputeExpiry(t *testing.T) {
	fixedNow := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc := &Service{
		pool:          (*pgxpool.Pool)(nil),
		now:           func() time.Time { return fixedNow },
		SlidingWindow: 7 * 24 * time.Hour,
		MaxLifetime:   30 * 24 * time.Hour,
	}

	t.Run("fresh session: now + sliding", func(t *testing.T) {
		got := svc.computeExpiry(fixedNow)
		want := fixedNow.Add(7 * 24 * time.Hour)
		if !got.Equal(want) {
			t.Errorf("got %v want %v", got, want)
		}
	})

	t.Run("near cap: now + sliding still inside max", func(t *testing.T) {
		// Session created 24 days ago: max cap is created+30d = 6d from now.
		// Sliding (7d) would push past, so we expect cap not sliding.
		created := fixedNow.Add(-24 * 24 * time.Hour)
		got := svc.computeExpiry(created)
		want := created.Add(30 * 24 * time.Hour) // = fixedNow + 6d
		if !got.Equal(want) {
			t.Errorf("got %v want %v (sliding should be capped)", got, want)
		}
	})

	t.Run("comfortably inside cap: sliding wins", func(t *testing.T) {
		created := fixedNow.Add(-5 * 24 * time.Hour)
		got := svc.computeExpiry(created)
		want := fixedNow.Add(7 * 24 * time.Hour)
		if !got.Equal(want) {
			t.Errorf("got %v want %v", got, want)
		}
	})

	t.Run("at max: clamped to created+max", func(t *testing.T) {
		// Created exactly 30 days ago → max cap is now.
		created := fixedNow.Add(-30 * 24 * time.Hour)
		got := svc.computeExpiry(created)
		want := created.Add(30 * 24 * time.Hour)
		if !got.Equal(want) {
			t.Errorf("got %v want %v", got, want)
		}
		if !got.Equal(fixedNow) {
			t.Errorf("at-max cap should equal now, got %v want %v", got, fixedNow)
		}
	})
}
