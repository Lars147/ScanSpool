package scanner

import (
	"context"
	"testing"
)

func TestBackgroundReconnectCannotEnterOwnedOperation(t *testing.T) {
	guard := NewScanGuard()
	release, ok := guard.TryAcquire()
	if !ok {
		t.Fatal("cannot own operation")
	}
	defer release()
	// An invalid address fails discovery before any network request if this probe
	// accidentally enters Connect; its recorded error exposes that regression.
	sc := New("not-an-ip", 0, 0, "", WithScanGuard(guard))
	sc.backgroundCheck(context.Background())
	if sc.ConnectionError() != "" {
		t.Fatal("reconnect entered owned operation", sc.ConnectionError())
	}
	if !guard.Busy() {
		t.Fatal("reconnect released another operation's guard")
	}
}
