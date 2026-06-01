package query

import (
	"context"
	"fmt"
)

// Schema is the public catalog exposed by /api/v1/.../schema and, later, MCP.
// It is derived only from the executor's table allowlist.
type Schema struct {
	Tables []SchemaTable `json:"tables"`
}

type SchemaTable struct {
	Name    string   `json:"name"`
	Columns []Column `json:"columns"`
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
		out.Tables = append(out.Tables, SchemaTable{Name: table.Name, Columns: cols})
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
		cols = append(cols, Column{Name: name, Type: typ})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse describe rows %s: %w", table.Name, err)
	}
	return cols, nil
}
