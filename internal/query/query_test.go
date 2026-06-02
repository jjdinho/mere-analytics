package query

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

type contextKey struct{}

type captureQuerier struct {
	t     *testing.T
	query string
}

func (q *captureQuerier) QueryContext(ctx context.Context, sqlText string, _ ...any) (*sql.Rows, error) {
	q.t.Helper()
	if got := ctx.Value(contextKey{}); got != "request-context" {
		q.t.Fatalf("request context value missing in ClickHouse QueryContext: %v", got)
	}
	q.query = sqlText
	return nil, errors.New("stop")
}

func TestExecutorPassesRequestContextAndUnmodifiedSQL(t *testing.T) {
	q := &captureQuerier{t: t}
	exec := NewExecutor(q, "analytics")
	ctx := context.WithValue(context.Background(), contextKey{}, "request-context")
	sqlText := "SELECT count() FROM events"

	_, err := exec.Collect(ctx, "00000000-0000-0000-0000-000000000001", sqlText, 10)
	if err == nil {
		t.Fatal("expected fake query error")
	}
	if q.query != sqlText {
		t.Fatalf("query was modified: got %q want %q", q.query, sqlText)
	}
}

func TestAdditionalTableFilters(t *testing.T) {
	exec := NewExecutor(&captureQuerier{t: t}, "analytics")
	got := exec.additionalTableFilters("00000000-0000-0000-0000-000000000001")
	for _, want := range []string{
		"'analytics.events_raw_v1'",
		"'analytics.identity_links_v1'",
		"'analytics.persons_state'",
		"'analytics.sessions_state'",
		"'analytics.identity_links_mv'",
		"'analytics.persons_mv'",
		"'analytics.sessions_mv'",
		"project_id = ''00000000-0000-0000-0000-000000000001''",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("additional_table_filters %q missing %q", got, want)
		}
	}
}

func TestExecutorDefaultLimits(t *testing.T) {
	exec := NewExecutor(&captureQuerier{t: t}, "analytics")
	if exec.MaxExecutionTime != 60 {
		t.Fatalf("MaxExecutionTime default = %d, want 60", exec.MaxExecutionTime)
	}
	if exec.MaxResultRows != 1000 {
		t.Fatalf("MaxResultRows default = %d, want 1000", exec.MaxResultRows)
	}
}
