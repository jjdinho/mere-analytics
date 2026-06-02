package query

import (
	"context"
	"fmt"
)

// Schema is the public catalog exposed by /api/v1/.../schema and MCP. It is
// derived from the executor's table allowlist and enriched with curated
// descriptions so an agent (or human) can build effective queries without
// guessing what each table and column means.
type Schema struct {
	Tables []SchemaTable `json:"tables"`
}

type SchemaTable struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Columns     []Column `json:"columns"`
}

// tableDescriptions and columnDescriptions are the curated catalog text. Column
// descriptions are keyed by column name (names are consistent across the three
// public views), matching how the columns are discovered via DESCRIBE TABLE.
var tableDescriptions = map[string]string{
	"events":   "Analytics events captured from your application. A view over the raw landing table joined to the identity map, so a late $identify resolves a user's older anonymous events.",
	"persons":  "Unique users/persons — one row per resolved identity, with first/last seen timestamps and lifetime counts.",
	"sessions": "User sessions — one row per session_id, with start/end timestamps, duration, and event counts.",
}

var columnDescriptions = map[string]string{
	"project_id":    "The project that owns this row. Scoped automatically to your project; you never see other projects' data and never need a project_id filter.",
	"event":         "The event name, e.g. $pageview or button_click.",
	"distinct_id":   "Resolved user identity: the linked user_id when known, otherwise the anonymous_id.",
	"timestamp":     "When the event occurred (UTC), supplied at ingest.",
	"session_id":    "Session identifier supplied by the SDK/caller.",
	"properties":    "Event properties stored as a JSON string. Read fields with ClickHouse JSONExtract* functions, e.g. JSONExtractString(properties, '$timezone').",
	"extras":        "Additional ingest payload stored as a JSON string. Read fields with JSONExtract* functions.",
	"received_at":   "When the server received the event (UTC).",
	"first_seen":    "Timestamp of the person's first event (UTC).",
	"last_seen":     "Timestamp of the person's most recent event (UTC).",
	"event_count":   "Total number of events.",
	"session_count": "Approximate number of distinct sessions.",
	"timezone":      "Most recent non-empty properties.$timezone value seen.",
	"started_at":    "Timestamp of the session's first event (UTC).",
	"ended_at":      "Timestamp of the session's last event (UTC).",
	"duration_ms":   "Session duration in milliseconds (ended_at − started_at), computed at read time.",
}

// schemaColumn pairs a DESCRIBE'd name/type with its curated description.
// Unknown columns simply carry an empty description (omitted on the wire).
func schemaColumn(name, typ string) Column {
	return Column{Name: name, Type: typ, Description: columnDescriptions[name]}
}

// SchemaProvider reads ClickHouse metadata for the allowlisted query tables.
type SchemaProvider struct {
	db     Querier
	tables []Table
}

func NewSchemaProvider(db Querier, exec *Executor) *SchemaProvider {
	return &SchemaProvider{db: db, tables: exec.Tables()}
}

func (p *SchemaProvider) Schema(ctx context.Context) (Schema, error) {
	out := Schema{Tables: make([]SchemaTable, 0, len(p.tables))}
	for _, table := range p.tables {
		cols, err := p.describeTable(ctx, table)
		if err != nil {
			return Schema{}, err
		}
		out.Tables = append(out.Tables, SchemaTable{
			Name:        table.Name,
			Description: tableDescriptions[table.Name],
			Columns:     cols,
		})
	}
	return out, nil
}

func (p *SchemaProvider) describeTable(ctx context.Context, table Table) ([]Column, error) {
	rows, err := p.db.QueryContext(ctx, "DESCRIBE TABLE "+table.QualifiedName)
	if err != nil {
		return nil, fmt.Errorf("clickhouse describe %s: %w", table.Name, err)
	}
	defer rows.Close()

	var cols []Column
	names, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("clickhouse describe columns %s: %w", table.Name, err)
	}
	for rows.Next() {
		values := make([]any, len(names))
		scan := make([]any, len(names))
		for i := range values {
			scan[i] = &values[i]
		}
		if err := rows.Scan(scan...); err != nil {
			return nil, fmt.Errorf("clickhouse describe scan %s: %w", table.Name, err)
		}
		if len(values) < 2 {
			return nil, fmt.Errorf("clickhouse describe %s: expected at least 2 columns, got %d", table.Name, len(values))
		}
		name, _ := normalizeValue(values[0]).(string)
		typ, _ := normalizeValue(values[1]).(string)
		cols = append(cols, schemaColumn(name, typ))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse describe rows %s: %w", table.Name, err)
	}
	return cols, nil
}
