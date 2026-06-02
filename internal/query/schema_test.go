package query

import (
	"encoding/json"
	"strings"
	"testing"
)

// The public views' columns, per migrations/clickhouse. The schema endpoint is
// meant to describe every column an agent can SELECT, so each one must carry a
// curated description. This guards against a new column shipping description-less.
var knownColumnsByTable = map[string][]string{
	"events":   {"project_id", "event", "distinct_id", "timestamp", "session_id", "properties", "extras", "received_at"},
	"persons":  {"project_id", "distinct_id", "first_seen", "last_seen", "event_count", "session_count", "timezone"},
	"sessions": {"project_id", "session_id", "distinct_id", "started_at", "ended_at", "duration_ms", "event_count", "timezone"},
}

func TestTableDescriptionsCoverAllowlist(t *testing.T) {
	for table := range knownColumnsByTable {
		if strings.TrimSpace(tableDescriptions[table]) == "" {
			t.Errorf("table %q has no description", table)
		}
	}
}

func TestColumnDescriptionsCoverKnownColumns(t *testing.T) {
	for table, cols := range knownColumnsByTable {
		for _, col := range cols {
			if strings.TrimSpace(columnDescriptions[col]) == "" {
				t.Errorf("column %q (in %s) has no description", col, table)
			}
		}
	}
}

func TestSchemaColumnAttachesDescription(t *testing.T) {
	col := schemaColumn("event", "LowCardinality(String)")
	if col.Description == "" {
		t.Fatalf("known column got empty description: %+v", col)
	}
	unknown := schemaColumn("not_a_real_column", "String")
	if unknown.Description != "" {
		t.Fatalf("unknown column should have no description, got %q", unknown.Description)
	}
}

// The wire shape must include description when present and omit it entirely when
// absent — query result columns reuse Column and must not grow a null field.
func TestColumnJSONOmitsEmptyDescription(t *testing.T) {
	bare, err := json.Marshal(Column{Name: "n", Type: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bare), "description") {
		t.Errorf("description should be omitted when empty: %s", bare)
	}
	described, err := json.Marshal(Column{Name: "n", Type: "t", Description: "d"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(described), `"description":"d"`) {
		t.Errorf("description should be present when set: %s", described)
	}
}

func TestSchemaTableJSONOmitsEmptyDescription(t *testing.T) {
	bare, err := json.Marshal(SchemaTable{Name: "events"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bare), "description") {
		t.Errorf("description should be omitted when empty: %s", bare)
	}
}
