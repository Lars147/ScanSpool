// Package setup persists scanner connections and performs explicit PC enrollment.
package setup

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"scanspool/internal/persist"

	"scanspool/internal/vens"
)

type Connection struct {
	Host     string `json:"host"`
	Serial   string `json:"serial"`
	Model    string `json:"model"`
	Mode     string `json:"mode"` // pc or legacy
	Template []byte `json:"template,omitempty"`
	Identity string `json:"identity,omitempty"`
}

func (c Connection) Validate() error {
	if err := vens.ValidateScannerIP(c.Host); err != nil {
		return err
	}
	if c.Mode == "pc" {
		_, err := vens.MarshalPCReserveFromTemplate(c.Template, [8]byte{}, "127.0.0.1", 55265, time.Now())
		if err != nil {
			return err
		}
		if binary.BigEndian.Uint64(c.Template[108:116]) == 0 {
			return fmt.Errorf("saved host ID is zero")
		}
	} else if c.Mode != "legacy" {
		return fmt.Errorf("invalid saved connection mode")
	}
	return nil
}

type Store struct {
	mu         sync.RWMutex
	dir        string
	connection *Connection
}

func NewStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("persistent data directory required for setup")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir}
	data, err := os.ReadFile(filepath.Join(dir, "connection.json"))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var connection Connection
	if err := json.Unmarshal(data, &connection); err != nil {
		return nil, fmt.Errorf("invalid connection.json: %w", err)
	}
	if err := connection.Validate(); err != nil {
		return nil, err
	}
	s.connection = &connection
	return s, nil
}

func (s *Store) Get() *Connection {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.connection == nil {
		return nil
	}
	c := *s.connection
	c.Template = append([]byte(nil), c.Template...)
	return &c
}

func (s *Store) Save(c Connection) error {
	if err := c.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.writeJSON("connection.json", c)
	if err != nil && !persist.Committed(err) {
		return err
	}
	c.Template = append([]byte(nil), c.Template...)
	s.connection = &c
	return err
}

func (s *Store) HostID() (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.dir, "host-id")
	data, err := os.ReadFile(path)
	if err == nil {
		raw, err := hex.DecodeString(string(data))
		if err != nil || len(raw) != 8 || binary.BigEndian.Uint64(raw) == 0 {
			return 0, fmt.Errorf("invalid saved host-id")
		}
		return binary.BigEndian.Uint64(raw), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	var raw [8]byte
	for binary.BigEndian.Uint64(raw[:]) == 0 {
		if _, err := rand.Read(raw[:]); err != nil {
			return 0, err
		}
	}
	if err := s.write("host-id", []byte(hex.EncodeToString(raw[:]))); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(raw[:]), nil
}

func (s *Store) writeJSON(name string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return s.write(name, append(data, '\n'))
}

// write commits private state with the shared durable writer.
func (s *Store) write(name string, data []byte) error {
	if filepath.Base(name) != name {
		return fmt.Errorf("invalid state filename")
	}
	return persist.WriteFile(filepath.Join(s.dir, name), data)
}

// Clear removes a saved connection after recovering an interrupted first setup.
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(filepath.Join(s.dir, "connection.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.connection = nil
	return nil
}
