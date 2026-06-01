package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jjdinho/mere-analytics/internal/idgen"
	"github.com/jjdinho/mere-analytics/internal/postgres/db"
)

// flushItem groups events by the project they were submitted under so the CH
// insert can stamp project_id per row without re-walking envelopes.
type flushItem struct {
	projectID string
	event     Event
}

// runFlusher is the single-goroutine consumer of the envelope chan. It
// accumulates events until FlushEvents is reached or FlushInterval ticks,
// then attempts a CH insert. CH failure → DLQ insert; DLQ failure too →
// SetFatal(true). A clean CH insert always clears the fatal flag.
func (s *Service) runFlusher(ctx context.Context) {
	defer s.flusherWG.Done()

	ticker := time.NewTicker(s.opts.FlushInterval)
	defer ticker.Stop()

	disabledNoticeTicker := time.NewTicker(5 * time.Minute)
	defer disabledNoticeTicker.Stop()

	buf := make([]flushItem, 0, s.opts.FlushEvents)
	flush := func(reason string) {
		if len(buf) == 0 {
			return
		}
		s.attemptFlush(ctx, buf, reason)
		// drained counters use envelope-derived event count; here we
		// decrement per flushed event so Submit's accounting stays exact.
		s.chBuf.drained(len(buf))
		buf = buf[:0]
	}

	for {
		select {
		case env, ok := <-s.chBuf.ch:
			if !ok {
				// chan closed by Shutdown; drain whatever we accumulated
				// then exit.
				flush("close")
				return
			}
			for _, e := range env.Events {
				buf = append(buf, flushItem{projectID: env.ProjectID, event: e})
				if len(buf) >= s.opts.FlushEvents {
					flush("size")
				}
			}
		case <-ticker.C:
			flush("interval")
		case <-disabledNoticeTicker.C:
			if s.flags.IsDisabled() {
				s.logger.Warn("ingest disabled (kill switch active)")
			}
		case <-ctx.Done():
			flush("ctx")
			return
		}
	}
}

// attemptFlush tries one CH insert of the accumulated batch. On CH failure
// it serializes the batch to JSON + writes a single failed_events row. If
// both fail the fatal flag is set; the next clean CH flush will clear it.
func (s *Service) attemptFlush(ctx context.Context, items []flushItem, reason string) {
	if err := s.insertIntoClickHouse(ctx, items); err == nil {
		if s.flags.IsFatal() {
			s.flags.SetFatal(false)
			s.logger.Info("ingest fatal cleared", "events", len(items), "reason", reason)
		}
		return
	} else {
		s.logger.Warn("ch insert failed; writing DLQ row", "events", len(items), "err", err.Error(), "reason", reason)
		if dlqErr := s.writeDLQ(ctx, items, err.Error()); dlqErr != nil {
			s.flags.SetFatal(true)
			s.logger.Error("dlq insert failed; setting fatal",
				"events", len(items),
				"ch_err", err.Error(),
				"pg_err", dlqErr.Error())
		}
	}
}

// insertIntoClickHouse opens a single batch transaction, prepares the row
// INSERT, executes once per item, then commits. clickhouse-go/v2 buffers the
// prepared rows and ships them as a single block on Commit, so this is the
// canonical batch path.
func (s *Service) insertIntoClickHouse(ctx context.Context, items []flushItem) error {
	tx, err := s.ch.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ch begin: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO events_raw_v1
		(project_id, event, distinct_id, timestamp, session_id, properties, extras)
	`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("ch prepare: %w", err)
	}
	for _, it := range items {
		if _, err := stmt.ExecContext(ctx,
			it.projectID,
			it.event.Event,
			it.event.DistinctID,
			it.event.Timestamp,
			it.event.SessionID,
			string(it.event.Properties),
			string(it.event.Extras),
		); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return fmt.Errorf("ch exec: %w", err)
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("ch stmt close: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ch commit: %w", err)
	}
	return nil
}

// writeDLQ serializes the flush batch into a single failed_events row. The
// batch is recorded as a JSON array of {project_id, event} pairs so a future
// drain pass can replay the full payload, and a SIGTERM-induced residual
// dump can be counted in tests.
func (s *Service) writeDLQ(ctx context.Context, items []flushItem, lastError string) error {
	payload, err := json.Marshal(itemsToWire(items))
	if err != nil {
		return fmt.Errorf("dlq marshal: %w", err)
	}
	return s.q.InsertFailedEvent(ctx, db.InsertFailedEventParams{
		ID:           idgen.New(),
		BatchPayload: payload,
		LastError:    lastError,
	})
}

// wireItem is the on-disk shape of a DLQ row payload. project_id rides
// alongside the event so the drain replay can issue the same INSERT.
//
// DO NOT rename fields. Existing failed_events rows will be replayed against
// future binaries.
type wireItem struct {
	ProjectID string `json:"project_id"`
	Event     Event  `json:"event"`
}

func itemsToWire(items []flushItem) []wireItem {
	out := make([]wireItem, len(items))
	for i, it := range items {
		out[i] = wireItem{ProjectID: it.projectID, Event: it.event}
	}
	return out
}

func wireToItems(w []wireItem) []flushItem {
	out := make([]flushItem, len(w))
	for i, it := range w {
		out[i] = flushItem{projectID: it.ProjectID, event: it.Event}
	}
	return out
}
