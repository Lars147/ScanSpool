package scanner

import (
	"sync"
)

// ScanGuard owns an operation until its unique, idempotent lease is released.
// A timeout may cancel work, but cannot unlock work which has not finished.
type ScanGuard struct {
	mu      sync.Mutex
	active  bool
	closing bool
	idle    *sync.Cond
}

func NewScanGuard() *ScanGuard {
	g := &ScanGuard{}
	g.idle = sync.NewCond(&g.mu)
	return g
}

func (g *ScanGuard) TryAcquire() (func(), bool) {
	g.mu.Lock()
	if g.active || g.closing {
		g.mu.Unlock()
		return nil, false
	}
	g.active = true
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.active = false
			g.idle.Broadcast()
		})
	}, true
}

func (g *ScanGuard) Busy() bool { g.mu.Lock(); defer g.mu.Unlock(); return g.active }

// StopAndWait prevents new jobs and joins their complete scan/OCR/upload lifetime.
func (g *ScanGuard) StopAndWait() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closing = true
	for g.active {
		g.idle.Wait()
	}
}
