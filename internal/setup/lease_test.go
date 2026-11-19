//go:build linux || darwin

package setup

import "testing"

func TestServiceLeasePreventsConcurrentIdentities(t *testing.T) {
	dir := t.TempDir()
	first, err := AcquireLease(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := AcquireLease(dir); err == nil {
		second.Close()
		t.Fatal("second service acquired same state")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := AcquireLease(dir)
	if err != nil {
		t.Fatal("lease was not released")
	}
	next.Close()
}
