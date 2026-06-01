package ingest

import (
	"context"
	"time"
)

// Start launches the flusher + DLQ drain goroutines. Safe to call more than
// once; only the first call has effect.
func (s *Service) Start(ctx context.Context) {
	s.startOnce.Do(func() {
		s.flusherWG.Add(2)
		go s.runFlusher(ctx)
		go s.runDLQDrain(ctx)
	})
}

// Shutdown is SIGTERM phase 3. It signals the goroutines to stop, closes the
// envelope channel so the flusher can drain remaining work, and waits up to
// the provided ctx deadline. If the deadline fires while envelopes are still
// in flight, the residual is captured into a single failed_events row so
// nothing is silently dropped (Issue 6A).
//
// Safe to call more than once.
func (s *Service) Shutdown(ctx context.Context) error {
	var err error
	s.stopOnce.Do(func() {
		close(s.stopCh)
		// Close the chan input so the flusher's recv loop falls through to
		// the close branch once the buffered envelopes are drained.
		s.chBuf.close()

		done := make(chan struct{})
		go func() {
			s.flusherWG.Wait()
			close(done)
		}()

		select {
		case <-done:
			return
		case <-ctx.Done():
			err = s.captureResidual(context.Background())
		}
	})
	return err
}

// captureResidual is a best-effort dump of anything still sitting in the
// envelope channel after the SIGTERM grace expires. One DLQ row carries the
// whole residual blob so a follow-up drain can replay it. Uses a fresh
// background ctx because the caller's ctx has already fired.
func (s *Service) captureResidual(ctx context.Context) error {
	var items []flushItem
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case env, ok := <-s.chBuf.ch:
			if !ok {
				goto write
			}
			for _, e := range env.Events {
				items = append(items, flushItem{projectID: env.ProjectID, event: e})
			}
		default:
			goto write
		}
	}
write:
	if len(items) == 0 {
		return nil
	}
	if err := s.writeDLQ(ctx, items, "shutdown residual"); err != nil {
		s.logger.Error("residual dlq write failed", "events", len(items), "err", err)
		return err
	}
	s.logger.Warn("captured shutdown residual to dlq", "events", len(items))
	s.chBuf.drained(len(items))
	return nil
}
