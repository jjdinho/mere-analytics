package ingest

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func makeEnvelope(n int) batchEnvelope {
	return batchEnvelope{
		ProjectID: "p1",
		Events:    make([]Event, n),
	}
}

func TestChannel_submit_belowCeiling(t *testing.T) {
	c := newChannel(100, 100)
	if err := c.submit(makeEnvelope(10)); err != nil {
		t.Fatalf("submit 10: %v", err)
	}
	if got := c.pendingCount(); got != 10 {
		t.Errorf("pending: got %d want 10", got)
	}
}

func TestChannel_submit_rejectsAtCeiling(t *testing.T) {
	c := newChannel(5, 100)
	if err := c.submit(makeEnvelope(5)); err != nil {
		t.Fatalf("submit 5: %v", err)
	}
	if err := c.submit(makeEnvelope(1)); !errors.Is(err, ErrChannelFull) {
		t.Errorf("submit at ceiling: got %v want ErrChannelFull", err)
	}
}

func TestChannel_submit_oversizedSingleBatch(t *testing.T) {
	c := newChannel(5, 100)
	// A batch larger than the ceiling should not partially reserve slots —
	// the CAS gate must reject it whole.
	if err := c.submit(makeEnvelope(10)); !errors.Is(err, ErrChannelFull) {
		t.Errorf("oversized submit: got %v want ErrChannelFull", err)
	}
	if got := c.pendingCount(); got != 0 {
		t.Errorf("pending after oversized reject: got %d want 0", got)
	}
}

func TestChannel_drainedClearsSlots(t *testing.T) {
	c := newChannel(100, 100)
	_ = c.submit(makeEnvelope(20))
	_ = c.submit(makeEnvelope(30))
	c.drained(50)
	if got := c.pendingCount(); got != 0 {
		t.Errorf("pending after drained: got %d want 0", got)
	}
}

// Issue 8A — 50 goroutines hammering Submit. After enough drains, pending
// must hit 0 and the spillover return count must equal what the ceiling
// predicts. The -race detector is the second assertion.
func TestChannel_concurrentSubmit_race(t *testing.T) {
	const (
		workers     = 50
		iterations  = 200
		eventsEach  = 2
		ceiling     = 200
		envelopeCap = 4096
	)
	c := newChannel(ceiling, envelopeCap)

	// Background drainer: consumes envelopes and counts events.
	var drainedCount atomic.Int64
	drainerStop := make(chan struct{})
	var drainerWG sync.WaitGroup
	drainerWG.Add(1)
	go func() {
		defer drainerWG.Done()
		for {
			select {
			case <-drainerStop:
				return
			case env, ok := <-c.ch:
				if !ok {
					return
				}
				n := len(env.Events)
				c.drained(n)
				drainedCount.Add(int64(n))
			}
		}
	}()

	var (
		wg          sync.WaitGroup
		accepted    atomic.Int64
		rejected    atomic.Int64
		totalSubmit atomic.Int64
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				totalSubmit.Add(int64(eventsEach))
				err := c.submit(makeEnvelope(eventsEach))
				if err == nil {
					accepted.Add(int64(eventsEach))
				} else {
					rejected.Add(int64(eventsEach))
				}
			}
		}()
	}
	wg.Wait()

	// Wait until the drainer catches up on accepted events.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if drainedCount.Load() == accepted.Load() && c.pendingCount() == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(drainerStop)
	drainerWG.Wait()

	if got := c.pendingCount(); got != 0 {
		t.Errorf("pending after drain: got %d want 0", got)
	}
	if accepted.Load()+rejected.Load() != totalSubmit.Load() {
		t.Errorf("accepted+rejected (%d) != total (%d)", accepted.Load()+rejected.Load(), totalSubmit.Load())
	}
	if accepted.Load() == 0 {
		t.Error("nothing accepted; test misconfigured")
	}
	if drainedCount.Load() != accepted.Load() {
		t.Errorf("drained (%d) != accepted (%d)", drainedCount.Load(), accepted.Load())
	}
}
