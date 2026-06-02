package query

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// fakeQuerier records whether the executor reached the ClickHouse driver. The
// SETTINGS guard must reject before any DB call, so a rejected query must leave
// called=false.
type fakeQuerier struct{ called bool }

func (f *fakeQuerier) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	f.called = true
	return nil, errors.New("reached db")
}

func TestContainsSettingsClause(t *testing.T) {
	reject := []string{
		`SELECT 1 SETTINGS max_execution_time = 1`,
		`SELECT properties FROM events_raw_v1 WHERE event = 'pageview' SETTINGS additional_table_filters = {'analytics.events_raw_v1': '1=1'}`,
		`select * from events settings max_result_rows = 999`,
		"SELECT * FROM events\nSETTINGS\nmax_memory_usage = 1",
		`SELECT * FROM (SELECT * FROM events SETTINGS max_rows_to_read = 1)`,
		`WITH x AS (SELECT 1) SELECT * FROM events SETTINGS foo = 1`,
		`SELECT * FROM events /* note */ SETTINGS foo = 1`,
	}
	for _, sqlText := range reject {
		if !containsSettingsClause(sqlText) {
			t.Errorf("expected SETTINGS clause detected, missed: %q", sqlText)
		}
	}

	allow := []string{
		`SELECT count() FROM events`,
		`SELECT * FROM events WHERE event = 'Settings Opened'`,
		`SELECT * FROM events WHERE event = 'app settings changed'`,
		`SELECT properties FROM events WHERE properties LIKE '%settings%'`,
		`SELECT JSONExtractString(properties, 'settings') FROM events`,
		"SELECT properties FROM events -- adjust SETTINGS later\nWHERE event = 'x'",
		`SELECT properties FROM events /* SETTINGS go here */ WHERE event = 'x'`,
		`SELECT "settings" FROM events`,
		`SELECT settingsValue FROM events`,
	}
	for _, sqlText := range allow {
		if containsSettingsClause(sqlText) {
			t.Errorf("expected query allowed, wrongly flagged: %q", sqlText)
		}
	}
}

func TestExecutorRejectsUserSettingsBeforeDB(t *testing.T) {
	fq := &fakeQuerier{}
	exec := NewExecutor(fq, "analytics")
	_, err := exec.Collect(context.Background(), "proj", `SELECT 1 SETTINGS max_result_rows = 9999999`, 10)
	if !errors.Is(err, ErrSettingsNotAllowed) {
		t.Fatalf("err = %v, want ErrSettingsNotAllowed", err)
	}
	if fq.called {
		t.Error("ClickHouse was queried despite a user SETTINGS clause; guard must fail closed")
	}
}

func TestExecutorAllowsQueriesWithoutSettings(t *testing.T) {
	fq := &fakeQuerier{}
	exec := NewExecutor(fq, "analytics")
	_, err := exec.Collect(context.Background(), "proj", `SELECT * FROM events WHERE event = 'Settings Opened'`, 10)
	if errors.Is(err, ErrSettingsNotAllowed) {
		t.Fatalf("query without a SETTINGS clause was wrongly rejected: %v", err)
	}
	if !fq.called {
		t.Error("expected the executor to reach ClickHouse for a query with no SETTINGS clause")
	}
}
