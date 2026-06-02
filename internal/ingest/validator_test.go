package ingest

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func TestValidateBatch(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name           string
		events         []Event
		wantValid      int
		wantRejected   []ValidationError
		assertEventOne func(t *testing.T, e Event)
	}{
		{
			name: "all valid; defaults applied to properties+extras",
			events: []Event{
				{Event: "pageview", Timestamp: now},
				{Event: "click", Timestamp: now, Properties: json.RawMessage(`{"x":1}`)},
			},
			wantValid: 2,
			assertEventOne: func(t *testing.T, e Event) {
				if !bytes.Equal(e.Properties, []byte("{}")) {
					t.Errorf("Properties default: got %s want {}", e.Properties)
				}
				if !bytes.Equal(e.Extras, []byte("{}")) {
					t.Errorf("Extras default: got %s want {}", e.Extras)
				}
			},
		},
		{
			name: "empty event name rejected",
			events: []Event{
				{Event: "", Timestamp: now},
			},
			wantValid:    0,
			wantRejected: []ValidationError{{Index: 0, Reason: "event required"}},
		},
		{
			name: "zero timestamp rejected",
			events: []Event{
				{Event: "pageview", Timestamp: time.Time{}},
			},
			wantValid:    0,
			wantRejected: []ValidationError{{Index: 0, Reason: "timestamp required"}},
		},
		{
			name: "mixed valid + invalid preserves indices",
			events: []Event{
				{Event: "ok1", Timestamp: now},
				{Event: "", Timestamp: now},
				{Event: "ok2", Timestamp: now},
				{Event: "bad-ts"},
			},
			wantValid: 2,
			wantRejected: []ValidationError{
				{Index: 1, Reason: "event required"},
				{Index: 3, Reason: "timestamp required"},
			},
		},
		{
			name:      "empty batch → empty outputs",
			events:    nil,
			wantValid: 0,
		},
		{
			name: "explicit properties preserved verbatim",
			events: []Event{
				{Event: "x", Timestamp: now, Properties: json.RawMessage(`{"a":[1,2,3]}`)},
			},
			wantValid: 1,
			assertEventOne: func(t *testing.T, e Event) {
				if !bytes.Equal(e.Properties, []byte(`{"a":[1,2,3]}`)) {
					t.Errorf("Properties: got %s", e.Properties)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			valid, rejected := ValidateBatch(tt.events)
			if len(valid) != tt.wantValid {
				t.Errorf("valid count: got %d want %d", len(valid), tt.wantValid)
			}
			if len(rejected) != len(tt.wantRejected) {
				t.Fatalf("rejected count: got %d want %d (%v)", len(rejected), len(tt.wantRejected), rejected)
			}
			for i, want := range tt.wantRejected {
				if rejected[i] != want {
					t.Errorf("rejected[%d]: got %+v want %+v", i, rejected[i], want)
				}
			}
			if tt.assertEventOne != nil && len(valid) > 0 {
				tt.assertEventOne(t, valid[0])
			}
		})
	}
}
