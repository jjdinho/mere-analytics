// Package query owns the analytics read path. HTTP and MCP adapters call this
// package instead of constructing ClickHouse SQL settings themselves, so
// tenant isolation and query limits stay in one place.
package query

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"time"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
)

const (
	defaultMaxExecutionTime = 60
	defaultMaxMemoryUsage   = 4 * 1024 * 1024 * 1024
	defaultMaxResultRows    = 1_000
)

var (
	// ErrEmptySQL is returned before ClickHouse is touched when the request
	// body doesn't carry a query.
	ErrEmptySQL = errors.New("sql is required")

	// ErrRowLimitExceeded is used by the web playground sink to stop rendering
	// before a huge result set is buffered in memory.
	ErrRowLimitExceeded = errors.New("row limit exceeded")
)

// Querier is the subset of *sql.DB the executor needs. Tests use it to assert
// the original request context reaches the ClickHouse driver call.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Table is an allowlisted analytics object. Name is what users see in the
// schema response; QualifiedName is the safely quoted table/view for metadata
// reads.
type Table struct {
	Name          string
	QualifiedName string
}

// Column describes one query output or schema column.
type Column struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// Stats carries minimal execution metadata. It is intentionally small and
// transport-neutral so MCP can reuse it later.
type Stats struct {
	Rows      int64 `json:"rows"`
	ElapsedMS int64 `json:"elapsed_ms"`
}

// Result is the bounded in-memory shape used by the web playground. The API
// route streams instead.
type Result struct {
	Columns []Column
	Rows    [][]any
	Stats   Stats
}

// Executor runs user SQL through the readonly ClickHouse pool with project
// filters and bounded resource settings.
type Executor struct {
	db          Querier
	tables      []Table
	filterNames []string

	MaxExecutionTime int
	MaxMemoryUsage   uint64
	MaxResultRows    uint64
}

// NewExecutor builds an executor over the readonly ClickHouse pool. database
// is usually "analytics". The public allowlist exposes only the curated
// events/persons/sessions surface; filterNames holds the hidden physical
// tables those views read so tenant isolation still anchors to real storage.
func NewExecutor(db Querier, database string) *Executor {
	if strings.TrimSpace(database) == "" {
		database = "analytics"
	}
	table := func(name string) Table {
		return Table{
			Name:          name,
			QualifiedName: quoteIdent(database) + "." + quoteIdent(name),
		}
	}
	return &Executor{
		db:     db,
		tables: []Table{table("events"), table("persons"), table("sessions")},
		filterNames: []string{
			database + "." + "events_raw_v1",
			database + "." + "identity_links_v1",
			database + "." + "persons_state",
			database + "." + "sessions_state",
			database + "." + "identity_links_mv",
			database + "." + "persons_mv",
			database + "." + "sessions_mv",
		},
		MaxExecutionTime: defaultMaxExecutionTime,
		MaxMemoryUsage:   defaultMaxMemoryUsage,
		MaxResultRows:    defaultMaxResultRows,
	}
}

// Tables returns a defensive copy of the queryable table allowlist.
func (e *Executor) Tables() []Table {
	out := make([]Table, len(e.tables))
	copy(out, e.tables)
	return out
}

// StreamJSON writes the API response envelope incrementally:
//
//	{"columns":[...],"rows":[...],"stats":{...}}
//
// Rows are emitted as arrays in column order. The caller must set status and
// Content-Type before calling if it wants headers written early.
func (e *Executor) StreamJSON(ctx context.Context, projectID, sqlText string, w io.Writer) (Stats, error) {
	return e.run(ctx, projectID, sqlText, &jsonSink{w: w})
}

// Collect runs a query and buffers up to maxRows rows for the web playground.
func (e *Executor) Collect(ctx context.Context, projectID, sqlText string, maxRows int) (Result, error) {
	sink := &collectSink{maxRows: maxRows}
	stats, err := e.run(ctx, projectID, sqlText, sink)
	if err != nil {
		return Result{}, err
	}
	return Result{Columns: sink.columns, Rows: sink.rows, Stats: stats}, nil
}

func (e *Executor) run(ctx context.Context, projectID, sqlText string, sink sink) (Stats, error) {
	if strings.TrimSpace(sqlText) == "" {
		return Stats{}, ErrEmptySQL
	}
	start := time.Now()
	rows, err := e.db.QueryContext(e.contextWithSettings(ctx, projectID), sqlText)
	if err != nil {
		return Stats{}, fmt.Errorf("clickhouse query: %w", err)
	}
	defer rows.Close()

	names, err := rows.Columns()
	if err != nil {
		return Stats{}, fmt.Errorf("clickhouse columns: %w", err)
	}
	types, err := rows.ColumnTypes()
	if err != nil {
		return Stats{}, fmt.Errorf("clickhouse column types: %w", err)
	}
	columns := make([]Column, len(names))
	for i, name := range names {
		typ := ""
		if i < len(types) {
			typ = types[i].DatabaseTypeName()
		}
		columns[i] = Column{Name: name, Type: typ}
	}
	if err := sink.Begin(columns); err != nil {
		return Stats{}, err
	}

	values := make([]any, len(columns))
	scan := make([]any, len(columns))
	for i := range values {
		scan[i] = &values[i]
	}

	var count int64
	for rows.Next() {
		for i := range values {
			values[i] = nil
		}
		if err := rows.Scan(scan...); err != nil {
			return Stats{}, fmt.Errorf("clickhouse scan: %w", err)
		}
		row := make([]any, len(values))
		for i, v := range values {
			row[i] = normalizeValue(v)
		}
		if err := sink.Row(row); err != nil {
			return Stats{}, err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return Stats{}, fmt.Errorf("clickhouse rows: %w", err)
	}
	stats := Stats{Rows: count, ElapsedMS: time.Since(start).Milliseconds()}
	if err := sink.End(stats); err != nil {
		return Stats{}, err
	}
	return stats, nil
}

func (e *Executor) contextWithSettings(ctx context.Context, projectID string) context.Context {
	return chdriver.Context(ctx, chdriver.WithSettings(chdriver.Settings{
		"additional_table_filters": e.additionalTableFilters(projectID),
		"max_execution_time":       e.MaxExecutionTime,
		"max_memory_usage":         e.MaxMemoryUsage,
		"max_result_rows":          e.MaxResultRows,
	}))
}

func (e *Executor) additionalTableFilters(projectID string) string {
	parts := make([]string, 0, len(e.filterNames))
	filter := "project_id = " + quoteString(projectID)
	for _, name := range e.filterNames {
		parts = append(parts, quoteString(name)+": "+quoteString(filter))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

type sink interface {
	Begin([]Column) error
	Row([]any) error
	End(Stats) error
}

type jsonSink struct {
	w     io.Writer
	first bool
}

func (s *jsonSink) Begin(columns []Column) error {
	s.first = true
	b, err := json.Marshal(columns)
	if err != nil {
		return err
	}
	_, err = s.w.Write([]byte(`{"columns":` + string(b) + `,"rows":[`))
	return err
}

func (s *jsonSink) Row(row []any) error {
	if !s.first {
		if _, err := s.w.Write([]byte(",")); err != nil {
			return err
		}
	}
	s.first = false
	b, err := json.Marshal(row)
	if err != nil {
		return err
	}
	_, err = s.w.Write(b)
	return err
}

func (s *jsonSink) End(stats Stats) error {
	b, err := json.Marshal(stats)
	if err != nil {
		return err
	}
	_, err = s.w.Write([]byte(`],"stats":` + string(b) + `}`))
	return err
}

type collectSink struct {
	maxRows int
	columns []Column
	rows    [][]any
}

func (s *collectSink) Begin(columns []Column) error {
	s.columns = append([]Column(nil), columns...)
	return nil
}

func (s *collectSink) Row(row []any) error {
	if s.maxRows > 0 && len(s.rows) >= s.maxRows {
		return ErrRowLimitExceeded
	}
	copied := append([]any(nil), row...)
	s.rows = append(s.rows, copied)
	return nil
}

func (s *collectSink) End(Stats) error { return nil }

func normalizeValue(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	case []byte:
		return string(x)
	}
	rv := reflect.ValueOf(v)
	if rv.IsValid() && rv.Kind() == reflect.Array && rv.Type().Elem().Kind() == reflect.Uint8 {
		buf := make([]byte, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			buf[i] = byte(rv.Index(i).Uint())
		}
		if len(buf) == 16 {
			return formatUUID(buf)
		}
		return string(buf)
	}
	return v
}

func formatUUID(b []byte) string {
	if len(b) != 16 {
		return string(b)
	}
	var sb strings.Builder
	sb.Grow(36)
	for i, c := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			sb.WriteByte('-')
		}
		sb.WriteString(strconv.FormatUint(uint64(c>>4), 16))
		sb.WriteString(strconv.FormatUint(uint64(c&0x0f), 16))
	}
	return sb.String()
}

func quoteIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

func quoteString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
