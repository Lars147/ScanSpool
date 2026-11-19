// Package daemon manages one system-wide Linux receiver through systemd.
// The receiver itself remains the foreground run command.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"scanspool/internal/control"
)

const UnitName = "scanspool.service"
const managedBy = "scanspool/systemd-v1"

type Paths struct {
	Binary, Unit, Manifest, Lock string
}

type Installation struct {
	ManagedBy  string `json:"managedBy"`
	User       string `json:"user"`
	UID        string `json:"uid"`
	GID        string `json:"gid"`
	Home       string `json:"home"`
	DataDir    string `json:"dataDir"`
	BinaryHash string `json:"binaryHash"`
	UnitHash   string `json:"unitHash"`
}

type State struct {
	Installed bool   `json:"installed"`
	Enabled   bool   `json:"enabled"`
	Active    string `json:"active"`
	User      string `json:"user,omitempty"`
	DataDir   string `json:"dataDir,omitempty"`
}

type runner func(context.Context, bool, string, ...string) (string, error)

type Manager struct {
	paths        Paths
	run          runner
	platform     func() error
	uid          func() int
	lookup       func(string) (*user.User, error)
	executable   func() (string, error)
	checkFile    func(string, os.FileInfo) error
	checkParents func(string) error
	lock         func(string) (io.Closer, error)
	probe        func(context.Context, string) error
	write        func(string, []byte, os.FileMode) error
	readyTimeout time.Duration
}

func New() *Manager {
	return &Manager{
		paths: Paths{"/usr/local/bin/scanspool", "/etc/systemd/system/" + UnitName, "/etc/scanspool/service.json", "/run/scanspool-service.lock"},
		run:   runCommand, platform: checkPlatform, uid: os.Geteuid, lookup: user.Lookup,
		executable: os.Executable, checkFile: checkOwnedFile, checkParents: checkOwnedParents, lock: lockInstallation,
		probe: func(ctx context.Context, dir string) error {
			c, err := control.Attach(ctx, dir)
			if err != nil {
				return err
			}
			defer c.Close()
			status, err := c.Status(ctx)
			if err == nil && !status.Running {
				return fmt.Errorf("receiver is not running")
			}
			return err
		},
		readyTimeout: 30 * time.Second,
		write:        atomicFile,
	}
}

func runCommand(ctx context.Context, stream bool, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stream {
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		return "", cmd.Run()
	}
	data, err := cmd.CombinedOutput()
	if err != nil {
		return string(data), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(data)))
	}
	return string(data), nil
}

func cleanValue(value string) bool {
	return value != "" && strings.IndexFunc(value, unicode.IsControl) < 0
}

// canonical also resolves symlinked parents when the leaf does not exist yet.
func canonical(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(abs)
	if parent == abs {
		return "", err
	}
	base, err := canonical(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, filepath.Base(abs)), nil
}

func (m *Manager) Read() (*Installation, error) {
	info, err := os.Lstat(m.paths.Manifest)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("service manifest is not a regular file")
	}
	if err := m.checkFile(m.paths.Manifest, info); err != nil {
		return nil, err
	}
	if err := m.checkParents(filepath.Dir(m.paths.Manifest)); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(m.paths.Manifest)
	if err != nil {
		return nil, err
	}
	var installed Installation
	if err := json.Unmarshal(data, &installed); err != nil {
		return nil, fmt.Errorf("invalid service manifest: %w", err)
	}
	if installed.ManagedBy != managedBy || !cleanValue(installed.User) || !cleanValue(installed.Home) || !cleanValue(installed.DataDir) || !filepath.IsAbs(installed.DataDir) || !filepath.IsAbs(installed.Home) || len(installed.BinaryHash) != 64 || len(installed.UnitHash) != 64 {
		return nil, fmt.Errorf("invalid or foreign service manifest")
	}
	uid, e1 := strconv.Atoi(installed.UID)
	gid, e2 := strconv.Atoi(installed.GID)
	if e1 != nil || e2 != nil || uid <= 0 || gid < 0 {
		return nil, fmt.Errorf("invalid service account in manifest")
	}
	return &installed, nil
}

// ResolveDirectory checks identity before the caller can create logs or locks.
// The manifest only changes the default for the installed receiver's account.
func (m *Manager) ResolveDirectory(explicit, environment, fallback string) (string, bool, error) {
	installed, err := m.Read()
	if err != nil {
		return "", false, err
	}
	dir := explicit
	if dir == "" {
		dir = environment
	}
	if dir == "" && installed != nil && strconv.Itoa(m.uid()) == installed.UID {
		dir = installed.DataDir
	}
	if dir == "" {
		dir = fallback
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", false, err
	}
	if installed == nil {
		return abs, false, nil
	}
	wrongUser := strconv.Itoa(m.uid()) != installed.UID
	if abs == installed.DataDir && wrongUser {
		return "", false, fmt.Errorf("service data belongs to user %s (UID %s); run receiver commands as that user without sudo", installed.User, installed.UID)
	}
	actual, err := canonical(abs)
	if err != nil {
		return "", false, err
	}
	// Installation stores a canonical path. Do not traverse another account's
	// private service home when resolving an unrelated user's default.
	matched := actual == installed.DataDir || abs == installed.DataDir
	if matched && wrongUser {
		return "", false, fmt.Errorf("service data belongs to user %s (UID %s); run receiver commands as that user without sudo", installed.User, installed.UID)
	}
	return abs, matched, nil
}

func (m *Manager) state(ctx context.Context, installed *Installation) (State, error) {
	s := State{Installed: installed != nil, Active: "not-installed"}
	if installed == nil {
		return s, nil
	}
	s.User, s.DataDir = installed.User, installed.DataDir
	output, err := m.run(ctx, false, "systemctl", "show", UnitName, "--property=LoadState", "--property=ActiveState", "--property=UnitFileState")
	if err != nil {
		return s, err
	}
	values := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			values[k] = v
		}
	}
	s.Active = values["ActiveState"]
	if values["LoadState"] == "not-found" && s.Active != "active" && s.Active != "activating" && s.Active != "deactivating" {
		s.Active = "missing-unit"
	}
	if s.Active == "" {
		return s, fmt.Errorf("systemctl returned no service state")
	}
	s.Enabled = values["UnitFileState"] == "enabled"
	return s, nil
}

func (m *Manager) Status(ctx context.Context) (State, error) {
	if err := m.platform(); err != nil {
		return State{}, err
	}
	installed, err := m.Read()
	if err != nil {
		return State{}, err
	}
	return m.state(ctx, installed)
}

func (m *Manager) Logs(ctx context.Context, follow bool) error {
	if err := m.platform(); err != nil {
		return err
	}
	args := []string{"--unit=" + UnitName, "--lines=100", "--no-pager"}
	if follow {
		args = append(args, "--follow")
	}
	_, err := m.run(ctx, true, "journalctl", args...)
	if err != nil {
		return fmt.Errorf("read service journal (administrator or journal access may be required): %w", err)
	}
	return nil
}

func (m *Manager) mutation() (io.Closer, error) {
	if err := m.platform(); err != nil {
		return nil, err
	}
	if m.uid() != 0 {
		return nil, fmt.Errorf("service changes require administrator rights; rerun this service command with sudo")
	}
	return m.lock(m.paths.Lock)
}

func (m *Manager) waitReady(ctx context.Context, installed *Installation) error {
	ctx, cancel := context.WithTimeout(ctx, m.readyTimeout)
	defer cancel()
	var last error
	for {
		s, err := m.state(ctx, installed)
		if err != nil {
			return err
		}
		if s.Active == "failed" || s.Active == "inactive" || s.Active == "missing-unit" {
			return fmt.Errorf("receiver failed to start (%s); inspect scanspool service logs", s.Active)
		}
		if s.Active == "active" {
			if last = m.probe(ctx, installed.DataDir); last == nil {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("receiver not ready; inspect scanspool service logs: %w", errors.Join(ctx.Err(), last))
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (m *Manager) Control(ctx context.Context, action string) error {
	if action != "start" && action != "stop" && action != "restart" {
		return fmt.Errorf("unknown service action %q", action)
	}
	lock, err := m.mutation()
	if err != nil {
		return err
	}
	defer lock.Close()
	installed, err := m.Read()
	if err != nil {
		return err
	}
	if installed == nil {
		return fmt.Errorf("service is not installed; use service install first")
	}
	if err := m.verifyFiles(installed); err != nil {
		return err
	}
	s, err := m.state(ctx, installed)
	if err != nil {
		return err
	}
	if action == "start" && (s.Active == "active" || s.Active == "activating") {
		return m.waitReady(ctx, installed)
	}
	if action != "stop" && s.Active != "active" && s.Active != "activating" {
		if err := m.probe(ctx, installed.DataDir); err == nil {
			return fmt.Errorf("another receiver uses the service data directory; close standalone setup/run before starting the service")
		} else if !errors.Is(err, control.ErrUnavailable) {
			return fmt.Errorf("cannot check existing receiver: %w", err)
		}
	}
	if _, err = m.run(ctx, false, "systemctl", action, UnitName); err != nil {
		return err
	}
	if action == "stop" {
		return nil
	}
	return m.waitReady(ctx, installed)
}
