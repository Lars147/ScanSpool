package config

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"scanspool/internal/persist"
	"scanspool/internal/vens"
)

var ErrConflict = errors.New("configuration changed; reopen the form")

type FTP struct {
	Host      string `json:"host"`
	User      string `json:"user"`
	Password  string `json:"password"`
	Directory string `json:"directory"`
}

func (f FTP) Validate() error {
	for _, value := range []string{f.Host, f.User, f.Password, f.Directory} {
		if strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return fmt.Errorf("FTP settings must not contain control characters")
		}
	}
	if strings.TrimSpace(f.Host) != f.Host || strings.Contains(f.Host, "/") {
		return fmt.Errorf("enter FTP host or host:port, without a URL scheme")
	}
	if host, port, err := net.SplitHostPort(f.Host); err == nil {
		number, err := strconv.Atoi(port)
		if host == "" || err != nil || number < 1 || number > 65535 {
			return fmt.Errorf("invalid FTP host or port")
		}
	} else if strings.Contains(f.Host, ":") && net.ParseIP(f.Host) == nil {
		return fmt.Errorf("invalid FTP host or port")
	}

	if f.Host == "" {
		return fmt.Errorf("enter an FTP host (optionally host:port)")
	}
	if strings.ContainsAny(f.Host, "/\\\r\n\x00") {
		return fmt.Errorf("FTP host must not contain a URL scheme or path")
	}
	host := f.Host
	if h, _, err := net.SplitHostPort(f.Host); err == nil {
		host = h
	}
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("invalid FTP host")
	}
	if strings.ContainsAny(f.User+f.Password+f.Directory, "\r\n\x00") {
		return fmt.Errorf("FTP values must not contain control characters")
	}
	if f.User == "" {
		return fmt.Errorf("enter an FTP user")
	}
	if f.Directory == "" || !strings.HasPrefix(f.Directory, "/") || path.Clean(f.Directory) != f.Directory {
		return fmt.Errorf("enter an absolute FTP directory, for example /consume")
	}
	return nil
}
func (f FTP) SameTarget(other FTP) bool {
	return f.Host == other.Host && f.User == other.User && f.Directory == other.Directory
}

type View struct {
	FTP            FTP    `json:"ftp"`
	Kind           string `json:"kind"`
	LocalDirectory string `json:"localDirectory,omitempty"`
	Revision       string `json:"revision"`
	HasPassword    bool   `json:"hasPassword"`
}
type Update struct {
	FTP            FTP    `json:"ftp"`
	Kind           string `json:"kind,omitempty"`
	LocalDirectory string `json:"localDirectory,omitempty"`
	Revision       string `json:"revision"`
	PasswordAction string `json:"passwordAction"` // keep or set
}
type Store struct {
	mu      sync.RWMutex
	path    string
	target  Destination
	profile vens.ScanConfig
}

func Open(dir string) (*Store, error) {
	store := &Store{path: filepath.Join(dir, "settings.json"), target: Destination{Kind: KindFTP, FTP: FTP{Directory: "/"}}}
	store.profile = vens.PCReferenceConfig()
	profile, err := os.ReadFile(profilePath(store.path))
	if err == nil {
		if err := json.Unmarshal(profile, &store.profile); err != nil {
			return nil, fmt.Errorf("invalid scan profile: %w", err)
		}
		if err := vens.ValidatePCParameters(store.profile); err != nil {
			return nil, fmt.Errorf("invalid scan profile: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	data, err := os.ReadFile(store.path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &store.target); err != nil {
		return nil, fmt.Errorf("invalid output settings: %w", err)
	}
	store.target.Kind = store.target.OutputKind()
	if store.target.Kind != KindFTP && store.target.Kind != KindLocal {
		return nil, fmt.Errorf("unknown output kind %q", store.target.Kind)
	}
	return store, nil
}
func (s *Store) Get() Destination { s.mu.RLock(); defer s.mu.RUnlock(); return s.target }
func revision(f Destination) string {
	data, _ := json.Marshal(f)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}
func view(f Destination) View {
	v := View{FTP: f.FTP, Kind: f.OutputKind(), LocalDirectory: f.LocalDirectory, Revision: revision(f), HasPassword: f.Password != ""}
	v.FTP.Password = ""
	return v
}
func (s *Store) View() View { s.mu.RLock(); defer s.mu.RUnlock(); return view(s.target) }
func (s *Store) Save(update Update) (View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if update.Revision != revision(s.target) {
		return View{}, ErrConflict
	}
	next := s.target
	next.Kind = (Destination{Kind: update.Kind}).OutputKind()
	if next.Kind == KindLocal {
		next.LocalDirectory = update.LocalDirectory
		if err := next.Validate(); err != nil {
			return View{}, err
		}
		next.LocalDirectory = filepath.Clean(next.LocalDirectory)
		if err := ValidateLocalDirectory(next.LocalDirectory, filepath.Join(filepath.Dir(s.path), "outbox")); err != nil {
			return View{}, err
		}
	} else if next.Kind == KindFTP {
		next.FTP = update.FTP
		switch update.PasswordAction {
		case "", "keep":
			if next.Password != "" {
				return View{}, fmt.Errorf("choose set to replace the password")
			}
			next.Password = s.target.Password
		case "set": // Empty is permitted for FTP accounts with an empty password.
		default:
			return View{}, fmt.Errorf("invalid password action")
		}
	}
	if err := next.Validate(); err != nil {
		return View{}, err
	}
	if next.Kind == KindLocal {
		if err := CheckLocalDirectory(next.LocalDirectory, filepath.Join(filepath.Dir(s.path), "outbox")); err != nil {
			return View{}, fmt.Errorf("cannot use local output folder %q: %w", next.LocalDirectory, err)
		}
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return View{}, err
	}
	err = persist.WriteFile(s.path, append(data, '\n'))
	if err == nil || persist.Committed(err) {
		s.target = next
	}
	return view(s.target), err
}
