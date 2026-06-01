package ingest

import "encoding/json"

// emptyObject is the JSON literal we substitute for missing properties/extras.
// Kept as a package-level []byte so callers don't allocate per event.
var emptyObject = json.RawMessage(`{}`)

// ValidationError reports a per-event problem inside a submitted batch. Index
// is the position within the request's events array (0-based) so the client
// can correlate.
type ValidationError struct {
	Index  int    `json:"index"`
	Reason string `json:"reason"`
}

// ValidateBatch returns a sanitized event slice + a per-index rejection list.
// Pure: no I/O, no clock reads on the request.
//
// Rules — minimal, lenient:
//   - `event` must be non-empty (the only required field besides timestamp).
//   - `timestamp` must not be the zero time.
//   - `properties` / `extras` default to `{}` if absent or empty so the
//     ClickHouse String column never lands a NULL.
//
// Distinct_id and session_id stay optional (Nullable in events_raw_v1).
func ValidateBatch(events []Event) (valid []Event, rejected []ValidationError) {
	valid = make([]Event, 0, len(events))
	for i, e := range events {
		if e.Event == "" {
			rejected = append(rejected, ValidationError{Index: i, Reason: "event required"})
			continue
		}
		if e.Timestamp.IsZero() {
			rejected = append(rejected, ValidationError{Index: i, Reason: "timestamp required"})
			continue
		}
		if len(e.Properties) == 0 {
			e.Properties = emptyObject
		}
		if len(e.Extras) == 0 {
			e.Extras = emptyObject
		}
		valid = append(valid, e)
	}
	return valid, rejected
}
