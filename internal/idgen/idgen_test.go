package idgen

import (
	"testing"

	"github.com/google/uuid"
)

func TestNew_isUUIDv7(t *testing.T) {
	id := New()
	parsed, err := uuid.Parse(id)
	if err != nil {
		t.Fatalf("New produced unparseable id %q: %v", id, err)
	}
	if got := parsed.Version(); got != 7 {
		t.Errorf("uuid version: got %d want 7 (UUID v7)", got)
	}
}

func TestNew_isUnique(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id := New()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id at iteration %d: %s", i, id)
		}
		seen[id] = struct{}{}
	}
}
