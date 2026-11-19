package setup

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"scanspool/internal/vens"
)

type fakeDevice struct {
	table       []byte
	failWrite   bool
	failProbe   bool
	failRestore bool
	writes      int
	modes       []uint32
}

type fakeSession struct{ device *fakeDevice }

func (s *fakeSession) ReadHosts() ([]byte, error) { return bytes.Clone(s.device.table), nil }
func (s *fakeSession) ReplaceHost(before, record []byte) error {
	if !bytes.Equal(before, s.device.table) {
		return errors.New("stale table")
	}
	s.device.writes++
	s.device.table = append([]byte{1, 0, 0, 0, 0, 0, 0, 0}, record...)
	if s.device.failWrite {
		return errors.New("lost registration acknowledgement")
	}
	return nil
}
func (s *fakeSession) RestoreHosts(before, candidate []byte) error {
	if s.device.failRestore {
		return errors.New("recovery unavailable")
	}
	s.device.table = bytes.Clone(before)
	return nil
}
func (s *fakeSession) Inquiry() error {
	if s.device.failProbe {
		return errors.New("normal session probe failed")
	}
	return nil
}
func (s *fakeSession) Close() error { return nil }

func configuredManager(t *testing.T) (*Manager, *fakeDevice, []byte) {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	before := make([]byte, 24)
	before[0] = 1
	binary.BigEndian.PutUint64(before[8:], 0x1122334455667788)
	device := &fakeDevice{table: bytes.Clone(before)}
	manager := NewManager(store)
	manager.find = func(context.Context, vens.DiscoveryOptions) (*vens.DeviceInfo, error) {
		return &vens.DeviceInfo{DeviceIP: "192.0.2.10", Name: "ScanSnap iX1300", Serial: "TEST000001", InterfaceVersion: 16}, nil
	}
	manager.open = func(_ context.Context, _ vens.DeviceInfo, template []byte, mode uint32) (hostSession, error) {
		device.modes = append(device.modes, mode)
		if binary.BigEndian.Uint64(template[108:116]) == 0 {
			t.Fatal("zero identity used")
		}
		return &fakeSession{device}, nil
	}
	return manager, device, before
}

func TestEnrollmentNeedsExplicitReplacementConsent(t *testing.T) {
	manager, device, before := configuredManager(t)
	err := manager.Connect(context.Background(), Request{IP: "192.0.2.10", Serial: "TEST000001"})
	if !errors.Is(err, ErrConfirmReplacement) {
		t.Fatalf("expected replacement consent, got %v", err)
	}
	if device.writes != 0 || !bytes.Equal(before, device.table) {
		t.Fatal("registration changed before consent")
	}
	if manager.store.Get() != nil || manager.Status().RecoveryRequired {
		t.Fatal("unconfirmed setup persisted a connection or pending write")
	}
}

func TestEnrollmentPersistsIdentityAfterNormalSessionVerification(t *testing.T) {
	manager, device, _ := configuredManager(t)
	if err := manager.Connect(context.Background(), Request{IP: "192.0.2.10", Serial: "TEST000001", ConfirmReplacement: true}); err != nil {
		t.Fatal(err)
	}
	if len(device.modes) != 2 || device.modes[0] != 2 || device.modes[1] != 0 {
		t.Fatalf("expected setup followed by new normal session: %v", device.modes)
	}
	connection := manager.store.Get()
	if connection == nil || !bytes.Equal(device.table[8:16], connection.Template[108:116]) {
		t.Fatal("saved connection does not match new registration")
	}
	reopened, err := NewStore(manager.store.dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reopened.Get().Template, connection.Template) {
		t.Fatal("identity did not survive reopening")
	}
	for _, name := range []string{"connection.json", "host-id"} {
		stat, err := os.Stat(filepath.Join(manager.store.dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if stat.Mode().Perm() != 0600 {
			t.Fatalf("%s not private: %v", name, stat.Mode())
		}
	}
	if manager.Status().RecoveryRequired {
		t.Fatal("successful setup still requires recovery")
	}
	backups, err := filepath.Glob(filepath.Join(manager.store.dir, "host-backup-*.json"))
	if err != nil || len(backups) != 1 {
		t.Fatal("exact prior table not retained privately")
	}
}

func TestEnrollmentRecoversAfterAmbiguousWriteAndProbeFailure(t *testing.T) {
	for _, failure := range []string{"write", "probe", "save"} {
		t.Run(failure, func(t *testing.T) {
			manager, device, before := configuredManager(t)
			device.failWrite = failure == "write"
			device.failProbe = failure == "probe"
			if failure == "save" {
				if err := os.Mkdir(filepath.Join(manager.store.dir, "connection.json"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := manager.Connect(context.Background(), Request{IP: "192.0.2.10", Serial: "TEST000001", ConfirmReplacement: true}); err == nil {
				t.Fatal("expected failed transaction")
			}
			if !bytes.Equal(before, device.table) {
				t.Fatal("old registration not restored after failure")
			}
			if manager.store.Get() != nil || manager.Status().RecoveryRequired {
				t.Fatal("failed enrollment became active or retained a resolved pending transaction")
			}
		})
	}
}

func TestInterruptedRecoverySurvivesRestart(t *testing.T) {
	manager, device, before := configuredManager(t)
	device.failWrite = true
	device.failRestore = true
	if err := manager.Connect(context.Background(), Request{IP: "192.0.2.10", Serial: "TEST000001", ConfirmReplacement: true}); err == nil {
		t.Fatal("expected failure")
	}
	store, err := NewStore(manager.store.dir)
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewManager(store)
	restarted.find = manager.find
	restarted.open = manager.open
	if !restarted.Status().RecoveryRequired {
		t.Fatal("interrupted write not detected after restart")
	}
	if err := restarted.Connect(context.Background(), Request{IP: "192.0.2.10", Serial: "TEST000001", ConfirmReplacement: true}); err == nil {
		t.Fatal("new enrollment allowed before recovery")
	}
	device.failRestore = false
	if err := restarted.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if restarted.Status().RecoveryRequired || !bytes.Equal(device.table, before) {
		t.Fatal("explicit recovery did not restore original state")
	}
}

func TestEnrollmentRefusesDifferentScannerAndMultipleEntries(t *testing.T) {
	manager, device, _ := configuredManager(t)
	if err := manager.Connect(context.Background(), Request{IP: "192.0.2.10", Serial: "DIFFERENT", ConfirmReplacement: true}); err == nil {
		t.Fatal("different device accepted")
	}
	device.table = make([]byte, 40)
	device.table[0] = 2
	if err := manager.Connect(context.Background(), Request{IP: "192.0.2.10", Serial: "TEST000001", ConfirmReplacement: true}); err == nil {
		t.Fatal("multi-entry registry accepted")
	}
	if device.writes != 0 {
		t.Fatal("unsafe registry was written")
	}
}
