package scanner

import (
	"testing"
	"time"
)

func TestScanGuardOwnedRelease(t *testing.T) {
	g := NewScanGuard()
	release, ok := g.TryAcquire()
	if !ok {
		t.Fatal("first acquire failed")
	}
	if _, ok := g.TryAcquire(); ok {
		t.Fatal("active job was not protected")
	}
	release()
	next, ok := g.TryAcquire()
	if !ok {
		t.Fatal("released job remained locked")
	}
	release() // An old/double release must not clear the new job.
	if !g.Busy() {
		t.Fatal("old owner released new job")
	}
	next()
}
func TestStopWaitsForLongJobAndRejectsNewWork(t *testing.T) {
	g := NewScanGuard()
	release, _ := g.TryAcquire()
	closed := make(chan struct{})
	go func() { g.StopAndWait(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("shutdown skipped active job")
	case <-time.After(10 * time.Millisecond):
	}
	if _, ok := g.TryAcquire(); ok {
		t.Fatal("new job admitted during shutdown")
	}
	release()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not join")
	}
	if _, ok := g.TryAcquire(); ok {
		t.Fatal("job admitted after shutdown")
	}
}
