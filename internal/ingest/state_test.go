package ingest

import (
	"sync"
	"testing"
)

func TestFlags_initialZero(t *testing.T) {
	f := &Flags{}
	if f.IsFatal() {
		t.Error("IsFatal: want false on init")
	}
	if f.IsDisabled() {
		t.Error("IsDisabled: want false on init")
	}
	if f.DLQDepth() != 0 {
		t.Errorf("DLQDepth: got %d want 0", f.DLQDepth())
	}
}

func TestFlags_setAndRead(t *testing.T) {
	f := &Flags{}
	f.SetFatal(true)
	if !f.IsFatal() {
		t.Error("IsFatal: want true after SetFatal(true)")
	}
	f.SetFatal(false)
	if f.IsFatal() {
		t.Error("IsFatal: want false after SetFatal(false)")
	}
	f.SetDisabled(true)
	if !f.IsDisabled() {
		t.Error("IsDisabled: want true after SetDisabled(true)")
	}
	f.SetDLQDepth(42)
	if f.DLQDepth() != 42 {
		t.Errorf("DLQDepth: got %d want 42", f.DLQDepth())
	}
}

// Race-free under concurrent set/read. The -race detector is the assertion.
func TestFlags_concurrentSetRead(t *testing.T) {
	f := &Flags{}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(2)
		go func(v bool) {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				f.SetFatal(v)
				f.SetDisabled(!v)
				f.SetDLQDepth(int64(j))
			}
		}(i%2 == 0)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				_ = f.IsFatal()
				_ = f.IsDisabled()
				_ = f.DLQDepth()
			}
		}()
	}
	wg.Wait()
}
