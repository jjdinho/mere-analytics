package extension

import (
	"context"
	"testing"
)

// The no-op defaults are the open-source build's wiring; assert each one's
// permissive contract so a future edit can't silently change what shipping
// without a wrapper means.

func TestAllowAll_AlwaysAllows(t *testing.T) {
	ok, retryAfter := AllowAll{}.Allow(context.Background(), LimitKey{Surface: "ingest"})
	if !ok {
		t.Error("AllowAll denied a request")
	}
	if retryAfter != 0 {
		t.Errorf("retryAfter: %v want 0", retryAfter)
	}
}

func TestUnlimited_AlwaysAllowsAnalysis(t *testing.T) {
	ok, reason := Unlimited{}.AllowAnalysis(context.Background(), "proj-1")
	if !ok {
		t.Error("Unlimited denied analysis")
	}
	if reason != "" {
		t.Errorf("reason: %q want empty", reason)
	}
}
