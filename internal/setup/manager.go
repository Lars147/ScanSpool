package setup

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"scanspool/internal/vens"
)

var ErrConfirmReplacement = errors.New("confirm replacing the scanner's current computer registration")

// Candidate excludes HostIDs and passwords from discovery/API responses.
type Candidate struct {
	IP               string `json:"ip"`
	Name             string `json:"name"`
	Serial           string `json:"serial"`
	PasswordRequired bool   `json:"passwordRequired"`
	Supported        bool   `json:"supported"`
	InUse            bool   `json:"inUse"`
}

type Request struct {
	IP                 string `json:"ip"`
	Serial             string `json:"serial"`
	Password           string `json:"password"`
	ConfirmReplacement bool   `json:"confirmReplacement"`
}

type Status struct {
	Configured       bool   `json:"configured"`
	Host             string `json:"host,omitempty"`
	Model            string `json:"model,omitempty"`
	Busy             bool   `json:"busy"`
	Error            string `json:"error,omitempty"`
	RecoveryRequired bool   `json:"recoveryRequired"`
}

type hostSession interface {
	ReadHosts() ([]byte, error)
	ReplaceHost([]byte, []byte) error
	RestoreHosts([]byte, []byte) error
	Inquiry() error
	Close() error
}

type pendingRegistration struct {
	Previous   *Connection `json:"previous,omitempty"`
	Connection Connection  `json:"connection"`
	Before     []byte      `json:"before"`
	Candidate  []byte      `json:"candidate"`
}

type Manager struct {
	store     *Store
	find      func(context.Context, vens.DiscoveryOptions) (*vens.DeviceInfo, error)
	open      func(context.Context, vens.DeviceInfo, []byte, uint32) (hostSession, error)
	mu        sync.Mutex
	busy      bool
	lastError string
	// Activate stops the old runtime and executes a connection change while
	// preventing concurrent scans. It then starts/restarts the saved connection.
	Activate func(context.Context, func() error) error
}

func NewManager(store *Store) *Manager {
	return &Manager{store: store, find: vens.FindScanner, open: func(ctx context.Context, info vens.DeviceInfo, template []byte, mode uint32) (hostSession, error) {
		return vens.OpenPCHostSession(ctx, info, template, mode)
	}}
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Status{Busy: m.busy, Error: m.lastError}
	if c := m.store.Get(); c != nil {
		s.Configured = true
		s.Host = c.Host
		s.Model = c.Model
	}
	_, err := os.Stat(filepath.Join(m.store.dir, "pending-registration.json"))
	s.RecoveryRequired = err == nil
	return s
}

func (m *Manager) Discover(ctx context.Context, ip string) ([]Candidate, error) {
	if ip != "" {
		if err := vens.ValidateScannerIP(ip); err != nil {
			return nil, err
		}
	}
	found, err := vens.FindScanners(ctx, vens.DiscoveryOptions{ScannerIP: ip, Token: vens.NewToken(), Timeout: 3 * time.Second})
	if err != nil {
		return nil, err
	}
	candidates := make([]Candidate, 0, len(found))
	for _, info := range found {
		candidates = append(candidates, Candidate{IP: info.DeviceIP, Name: info.Name, Serial: info.Serial,
			PasswordRequired: info.PasswordRequired, Supported: info.Name == "ScanSnap iX1300" && info.InterfaceVersion == 16,
			InUse: info.Paired || (info.ClientIP != "" && info.ClientIP != "0.0.0.0")})
	}
	return candidates, nil
}

func (m *Manager) operation(ctx context.Context, fn func() error) (err error) {
	m.mu.Lock()
	if m.busy {
		m.mu.Unlock()
		return fmt.Errorf("another setup operation is in progress")
	}
	m.busy = true
	m.lastError = ""
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.busy = false
		if err != nil {
			m.lastError = err.Error()
		}
	}()
	if m.Activate != nil {
		return m.Activate(ctx, fn)
	}
	return fn()
}

func (m *Manager) Connect(ctx context.Context, req Request) error {
	if err := vens.ValidateScannerIP(req.IP); err != nil {
		return err
	}
	if req.Serial == "" {
		return fmt.Errorf("select a discovered scanner first")
	}
	return m.operation(ctx, func() error {
		if m.Status().RecoveryRequired {
			return fmt.Errorf("recover the interrupted registration before connecting")
		}
		info, err := m.find(ctx, vens.DiscoveryOptions{ScannerIP: req.IP, Token: vens.NewToken(), Timeout: 5 * time.Second})
		if err != nil {
			return err
		}
		if info.Serial != req.Serial {
			return fmt.Errorf("selected scanner identity changed")
		}
		if info.Name != "ScanSnap iX1300" || info.InterfaceVersion != 16 {
			return fmt.Errorf("this release supports ScanSnap iX1300 interface 16")
		}

		hostID, err := m.store.HostID()
		if err != nil {
			return err
		}
		template, err := vens.PCReserveTemplate(*info, hostID, req.Password)
		if err != nil {
			return err
		}
		connection := Connection{Host: info.DeviceIP, Serial: info.Serial, Model: info.Name, Mode: "pc", Template: template}
		session, err := m.open(ctx, *info, template, 2)
		if err != nil {
			return err
		}
		before, err := session.ReadHosts()
		if err != nil {
			return errors.Join(err, session.Close())
		}
		if before[0] > 1 {
			return errors.Join(fmt.Errorf("setup refuses to replace multiple registered computers"), session.Close())
		}
		record := make([]byte, 16)
		binary.BigEndian.PutUint64(record, hostID)
		alreadyRegistered := len(before) == 24 && bytes.Equal(before[8:], record)
		if !alreadyRegistered && before[0] > 0 && !req.ConfirmReplacement {
			return errors.Join(ErrConfirmReplacement, session.Close())
		}
		pending := pendingRegistration{Previous: m.store.Get(), Connection: connection, Before: before, Candidate: record}
		if err := m.store.writeJSON("pending-registration.json", pending); err != nil {
			return errors.Join(err, session.Close())
		}
		backupName := fmt.Sprintf("host-backup-%d.json", time.Now().UnixNano())
		if err := m.store.writeJSON(backupName, pending); err != nil {
			return errors.Join(err, session.Close())
		}
		writeErr := session.ReplaceHost(before, record)
		closeErr := session.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			return m.recoverAfterFailure(err, pending)
		}
		// Verify the independent identity in a new normal session, not merely
		// the setup session in which the registration was written.
		checkInfo := *info
		checkInfo.Paired = false
		checkInfo.ClientIP = ""
		normal, err := m.open(ctx, checkInfo, template, 0)
		if err != nil {
			return m.recoverAfterFailure(err, pending)
		}
		probeErr := normal.Inquiry()
		if err := errors.Join(probeErr, normal.Close()); err != nil {
			return m.recoverAfterFailure(err, pending)
		}
		if err := m.store.Save(connection); err != nil {
			return m.recoverAfterFailure(err, pending)
		}
		if err := os.Remove(filepath.Join(m.store.dir, "pending-registration.json")); err != nil {
			return err
		}
		return nil
	})
}

func (m *Manager) recoverAfterFailure(cause error, pending pendingRegistration) error {
	// Request cancellation must not cancel restoring a scanner after a write.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.restore(ctx, pending); err != nil {
		return errors.Join(cause, fmt.Errorf("registration recovery failed; private backup retained: %w", err))
	}
	if err := m.restoreSavedConnection(pending.Previous); err != nil {
		return errors.Join(cause, fmt.Errorf("restore saved connection: %w", err))
	}
	if err := os.Remove(filepath.Join(m.store.dir, "pending-registration.json")); err != nil {
		return errors.Join(cause, err)
	}
	return fmt.Errorf("registration failed; original scanner record restored: %w", cause)
}

func (m *Manager) restore(ctx context.Context, pending pendingRegistration) error {
	info, err := m.find(ctx, vens.DiscoveryOptions{ScannerIP: pending.Connection.Host, Token: vens.NewToken(), Timeout: 5 * time.Second})
	if err != nil {
		return err
	}
	if info.Serial != pending.Connection.Serial {
		return fmt.Errorf("scanner identity changed; recovery stopped")
	}
	session, err := m.open(ctx, *info, pending.Connection.Template, 2)
	if err != nil {
		return err
	}
	err = session.RestoreHosts(pending.Before, pending.Candidate)
	return errors.Join(err, session.Close())
}

func (m *Manager) Recover(ctx context.Context) error {
	return m.operation(ctx, func() error {
		data, err := os.ReadFile(filepath.Join(m.store.dir, "pending-registration.json"))
		if err != nil {
			return err
		}
		var pending pendingRegistration
		if err := json.Unmarshal(data, &pending); err != nil {
			return err
		}
		if err := pending.Connection.Validate(); err != nil {
			return err
		}
		if err := m.restore(ctx, pending); err != nil {
			return err
		}
		if pending.Previous != nil {
			if err := m.store.Save(*pending.Previous); err != nil {
				return err
			}
		} else if err := m.store.Clear(); err != nil {
			return err
		}
		return os.Remove(filepath.Join(m.store.dir, "pending-registration.json"))
	})
}

func (m *Manager) restoreSavedConnection(previous *Connection) error {
	if previous != nil {
		return m.store.Save(*previous)
	}
	return m.store.Clear()
}
