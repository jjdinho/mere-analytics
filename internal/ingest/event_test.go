package ingest

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// parseObj decodes a JSON object into a map for order-independent comparison
// (json.Marshal sorts map keys, but we don't want the assertions to depend on
// that). An empty/nil blob decodes to an empty map.
func parseObj(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	if len(raw) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	return m
}

// Any top-level field that isn't first-class is folded into extras verbatim —
// the "lenient on extras" contract. No rejection, no migration needed.
func TestEvent_UnmarshalJSON_collectsUnknownFieldsIntoExtras(t *testing.T) {
	var e Event
	body := `{"event":"signup","timestamp":"2026-06-01T00:00:00Z","plan_tier":"pro","experiment_id":42}`
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if e.Event != "signup" {
		t.Errorf("Event: got %q want signup", e.Event)
	}
	want := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if !e.Timestamp.Equal(want) {
		t.Errorf("Timestamp: got %s want %s", e.Timestamp, want)
	}
	got := parseObj(t, e.Extras)
	wantExtras := map[string]any{"plan_tier": "pro", "experiment_id": float64(42)}
	if !reflect.DeepEqual(got, wantExtras) {
		t.Errorf("Extras: got %v want %v", got, wantExtras)
	}
}

// First-class fields are parsed into their own struct fields and must NOT leak
// into extras.
func TestEvent_UnmarshalJSON_firstClassFieldsExcludedFromExtras(t *testing.T) {
	var e Event
	body := `{
		"event":"click",
		"timestamp":"2026-06-01T00:00:00Z",
		"distinct_id":"u1",
		"session_id":"s1",
		"properties":{"path":"/"},
		"custom":"keep"
	}`
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if e.DistinctID == nil || *e.DistinctID != "u1" {
		t.Errorf("DistinctID: got %v", e.DistinctID)
	}
	if e.SessionID == nil || *e.SessionID != "s1" {
		t.Errorf("SessionID: got %v", e.SessionID)
	}
	if got := parseObj(t, e.Properties); !reflect.DeepEqual(got, map[string]any{"path": "/"}) {
		t.Errorf("Properties: got %v", got)
	}
	if got := parseObj(t, e.Extras); !reflect.DeepEqual(got, map[string]any{"custom": "keep"}) {
		t.Errorf("Extras: got %v want {custom:keep}", got)
	}
}

// A numeric timestamp is interpreted as epoch milliseconds (plan: "ISO 8601 or
// epoch ms").
func TestEvent_UnmarshalJSON_epochMillisTimestamp(t *testing.T) {
	var e Event
	ms := int64(1717200000000)
	if err := json.Unmarshal([]byte(`{"event":"x","timestamp":1717200000000}`), &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if want := time.UnixMilli(ms).UTC(); !e.Timestamp.Equal(want) {
		t.Errorf("Timestamp: got %s want %s", e.Timestamp, want)
	}
}

// A string timestamp is still parsed as ISO 8601 / RFC 3339 (regression).
func TestEvent_UnmarshalJSON_iso8601Timestamp(t *testing.T) {
	var e Event
	if err := json.Unmarshal([]byte(`{"event":"x","timestamp":"2026-06-01T12:30:45.123Z"}`), &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := time.Date(2026, 6, 1, 12, 30, 45, 123000000, time.UTC)
	if !e.Timestamp.Equal(want) {
		t.Errorf("Timestamp: got %s want %s", e.Timestamp, want)
	}
}

// A missing timestamp leaves the zero value so ValidateBatch can reject it.
func TestEvent_UnmarshalJSON_missingTimestampStaysZero(t *testing.T) {
	var e Event
	if err := json.Unmarshal([]byte(`{"event":"x"}`), &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !e.Timestamp.IsZero() {
		t.Errorf("Timestamp: got %s want zero", e.Timestamp)
	}
}

// An explicit extras object is honored as the base; stray top-level fields are
// merged on top.
func TestEvent_UnmarshalJSON_explicitExtrasMergedWithStrayFields(t *testing.T) {
	var e Event
	body := `{"event":"x","timestamp":"2026-06-01T00:00:00Z","extras":{"a":1},"b":2}`
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := parseObj(t, e.Extras)
	want := map[string]any{"a": float64(1), "b": float64(2)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Extras: got %v want %v", got, want)
	}
}

// The DLQ marshals validated events to JSON and replays them on drain, so an
// event must survive a marshal→unmarshal round-trip: stray fields stay in
// extras, properties keeps its {} default, and the timestamp instant is
// preserved.
func TestEvent_DLQRoundTrip(t *testing.T) {
	var e Event
	ms := int64(1717200000000)
	body := `{"event":"purchase","timestamp":1717200000000,"distinct_id":"u9","amount":9.99}`
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	valid, rejected := ValidateBatch([]Event{e})
	if len(valid) != 1 || len(rejected) != 0 {
		t.Fatalf("validate: valid=%d rejected=%v", len(valid), rejected)
	}

	data, err := json.Marshal(valid[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Event
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}

	if got.Event != "purchase" {
		t.Errorf("Event: got %q", got.Event)
	}
	if !got.Timestamp.Equal(time.UnixMilli(ms).UTC()) {
		t.Errorf("Timestamp: got %s", got.Timestamp)
	}
	if got.DistinctID == nil || *got.DistinctID != "u9" {
		t.Errorf("DistinctID: got %v", got.DistinctID)
	}
	if gotExtras := parseObj(t, got.Extras); !reflect.DeepEqual(gotExtras, map[string]any{"amount": 9.99}) {
		t.Errorf("Extras after round-trip: got %v want {amount:9.99}", gotExtras)
	}
	if gotProps := parseObj(t, got.Properties); !reflect.DeepEqual(gotProps, map[string]any{}) {
		t.Errorf("Properties after round-trip: got %v want {}", gotProps)
	}
}
