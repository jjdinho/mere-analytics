package ingest

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jjdinho/mere-analytics/internal/postgres/db"
)

// quarantineAttemptLimit is the retry budget per failed_events row. Hit it
// and the row is moved to the quarantined-forever pile.
const quarantineAttemptLimit = 20

// quarantineAge bounds how long a row can rattle around in the DLQ before
// it's quarantined regardless of attempt count. 24h matches the operator
// "pager-friendly" assumption: anything still stuck after a day is unlikely
// to spontaneously resolve.
const quarantineAge = 24 * time.Hour

// dlqDepthWarn is the soft threshold for an elevated-DLQ WARN log.
const dlqDepthWarn = 100

// dlqDepthError is the hard threshold for an ERROR-level log line.
const dlqDepthError = 10_000

// runDLQDrain is the goroutine that re-attempts CH inserts for failed_events
// rows and quarantines rows past the retry budget. It also refreshes
// Flags.DLQDepth after every pass for /healthz observability.
func (s *Service) runDLQDrain(ctx context.Context) {
	defer s.flusherWG.Done()

	ticker := time.NewTicker(s.opts.FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.drainOnce(ctx)
		}
	}
}

func (s *Service) drainOnce(ctx context.Context) {
	rows, err := s.q.ListFailedEventsForDrain(ctx, int32(s.opts.DLQDrainBatchLimit))
	if err != nil {
		s.logger.Warn("dlq list failed", "err", err)
		return
	}
	for _, row := range rows {
		s.processDLQRow(ctx, row)
	}
	depth, err := s.q.CountActiveFailedEvents(ctx)
	if err != nil {
		s.logger.Warn("dlq count failed", "err", err)
		return
	}
	s.flags.SetDLQDepth(depth)
	switch {
	case depth >= dlqDepthError:
		s.logger.Error("dlq depth high", "depth", depth)
	case depth >= dlqDepthWarn:
		s.logger.Warn("dlq depth elevated", "depth", depth)
	default:
		if depth > 0 || len(rows) > 0 {
			s.logger.Info("dlq depth", "depth", depth)
		}
	}
}

func (s *Service) processDLQRow(ctx context.Context, row db.FailedEvent) {
	if shouldQuarantine(row) {
		if err := s.q.QuarantineFailedEvent(ctx, row.ID); err != nil {
			s.logger.Warn("dlq quarantine failed", "id", row.ID, "err", err)
			return
		}
		s.logger.Warn("dlq row quarantined",
			"id", row.ID, "attempts", row.AttemptCount, "age", time.Since(row.CreatedAt.Time))
		return
	}
	var wire []wireItem
	if err := json.Unmarshal(row.BatchPayload, &wire); err != nil {
		// Unparseable payload — quarantine so we don't churn on it forever.
		s.logger.Warn("dlq row unparseable, quarantining", "id", row.ID, "err", err)
		_ = s.q.QuarantineFailedEvent(ctx, row.ID)
		return
	}
	items := wireToItems(wire)
	if err := s.insertIntoClickHouse(ctx, items); err != nil {
		bumpErr := s.q.IncrementFailedEventAttempt(ctx, db.IncrementFailedEventAttemptParams{
			ID:        row.ID,
			LastError: err.Error(),
		})
		if bumpErr != nil {
			s.logger.Warn("dlq bump failed", "id", row.ID, "err", bumpErr)
		}
		return
	}
	if err := s.q.DeleteFailedEvent(ctx, row.ID); err != nil {
		s.logger.Warn("dlq delete failed", "id", row.ID, "err", err)
		return
	}
	if s.flags.IsFatal() {
		s.flags.SetFatal(false)
		s.logger.Info("ingest fatal cleared via drain", "id", row.ID)
	}
}

// shouldQuarantine reports whether row's attempt budget or age has been
// blown. Called before the next retry — quarantine wins over insert so a
// boundary-condition row gets one final retry, not 21.
func shouldQuarantine(row db.FailedEvent) bool {
	if row.AttemptCount+1 >= quarantineAttemptLimit {
		return true
	}
	if !row.CreatedAt.Time.IsZero() && time.Since(row.CreatedAt.Time) >= quarantineAge {
		return true
	}
	return false
}
