package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func digest(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }

// Quoted unit arguments use systemd's escaping, not shell escaping.
func unitArgument(value string) string {
	r := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "%", "%%")
	return "\"" + r.Replace(value) + "\""
}

func execArgument(value string) string { return unitArgument(strings.ReplaceAll(value, "$", "$$")) }

// systemd 219 does not unescape WorkingDirectory or expand specifiers in
// RequiresMountsFor. Keep the supported path subset identical on old/new hosts.
func servicePath(value string) error {
	if !cleanValue(value) || !filepath.IsAbs(value) || strings.TrimSpace(value) != value || strings.ContainsAny(value, "%\"\\") {
		return fmt.Errorf("service paths must be absolute, without control characters, percent signs, double quotes, backslashes or trailing whitespace: %q", value)
	}
	return nil
}

func (m *Manager) unit(i *Installation) []byte {
	return []byte(fmt.Sprintf(`# Managed by %s. Reinstall with scanspool service install.
[Unit]
Description=ScanSpool receiver
Wants=network-online.target
After=network-online.target
RequiresMountsFor=%s

[Service]
Type=simple
User=%s
Group=%s
Environment=%s
WorkingDirectory=%s
ExecStart=%s run --data-dir %s
Restart=on-failure
RestartSec=5s
KillSignal=SIGTERM
TimeoutStopSec=120s
UMask=0022
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
`, managedBy, unitArgument(i.DataDir), i.UID, i.GID, unitArgument("HOME="+i.Home), i.DataDir, execArgument(m.paths.Binary), execArgument(i.DataDir)))
}

type savedFile struct {
	path   string
	data   []byte
	mode   os.FileMode
	exists bool
}

func (m *Manager) snapshot(path string) (savedFile, error) {
	s := savedFile{path: path}
	if err := m.checkParents(filepath.Dir(path)); err != nil {
		return s, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if !info.Mode().IsRegular() {
		return s, fmt.Errorf("refusing non-regular service file %s", path)
	}
	if err := m.checkFile(path, info); err != nil {
		return s, err
	}
	s.data, err = os.ReadFile(path)
	s.exists, s.mode = true, info.Mode().Perm()
	return s, err
}

func (m *Manager) verifyFiles(installed *Installation) error {
	for path, hash := range map[string]string{m.paths.Binary: installed.BinaryHash, m.paths.Unit: installed.UnitHash} {
		s, err := m.snapshot(path)
		if err != nil {
			return err
		}
		if s.exists && digest(s.data) != hash {
			return fmt.Errorf("refusing modified or foreign service file %s", path)
		}
	}
	return nil
}

func atomicFile(path string, data []byte, mode os.FileMode) error {
	if err := installParents(filepath.Dir(path)); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".scanspool-install-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// An administrator may use umask 077. Only directories created by this
// installer get public traversal; existing directories and user data stay as-is.
func installParents(path string) error {
	var missing []string
	for parent := path; ; parent = filepath.Dir(parent) {
		if _, err := os.Lstat(parent); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		missing = append(missing, parent)
	}
	for n := len(missing) - 1; n >= 0; n-- {
		if err := os.Mkdir(missing[n], 0755); err != nil {
			return err
		}
		if err := os.Chmod(missing[n], 0755); err != nil {
			return err
		}
	}
	return nil
}

func restoreFiles(files []savedFile) error {
	var failures []error
	for n := len(files) - 1; n >= 0; n-- {
		f := files[n]
		if f.exists {
			failures = append(failures, atomicFile(f.path, f.data, f.mode))
		} else {
			err := os.Remove(f.path)
			if !errors.Is(err, os.ErrNotExist) {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}

func (m *Manager) rollback(files []savedFile, wasEnabled bool) error {
	// Rollback must still work after the caller's context was cancelled.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var disableErr error
	if _, err := os.Lstat(m.paths.Unit); err == nil {
		_, disableErr = m.run(ctx, false, "systemctl", "disable", UnitName)
	}
	err := restoreFiles(files)
	_, reloadErr := m.run(ctx, false, "systemctl", "daemon-reload")
	var enableErr error
	if wasEnabled {
		_, enableErr = m.run(ctx, false, "systemctl", "enable", UnitName)
	}
	return errors.Join(err, disableErr, reloadErr, enableErr)
}

func (m *Manager) Install(ctx context.Context, username, directory string) error {
	lock, err := m.mutation()
	if err != nil {
		return err
	}
	defer lock.Close()
	installed, err := m.Read()
	if err != nil {
		return err
	}
	if installed != nil {
		if err := m.verifyFiles(installed); err != nil {
			return err
		}
		if username != "" && username != installed.User {
			return fmt.Errorf("changing service user requires uninstall and reinstall")
		}
		if directory != "" {
			if !filepath.IsAbs(directory) {
				return fmt.Errorf("--data-dir must be absolute")
			}
			requested, err := canonical(directory)
			if err != nil {
				return err
			}
			existing, err := canonical(installed.DataDir)
			if err != nil {
				return err
			}
			if requested != existing {
				return fmt.Errorf("changing service data directory requires uninstall and reinstall")
			}
		}
		username, directory = installed.User, installed.DataDir
	} else {
		if username == "" {
			username = os.Getenv("SUDO_USER")
		}
		if !cleanValue(username) {
			return fmt.Errorf("specify the existing receiver account with --user (or invoke through sudo)")
		}
		if !filepath.IsAbs(directory) || !cleanValue(directory) {
			return fmt.Errorf("first install requires an existing absolute --data-dir")
		}
	}
	account, err := m.lookup(username)
	if err != nil {
		return fmt.Errorf("look up receiver user: %w", err)
	}
	uid, e1 := strconv.Atoi(account.Uid)
	gid, e2 := strconv.Atoi(account.Gid)
	if e1 != nil || e2 != nil || uid <= 0 || gid < 0 || !cleanValue(account.Username) || !cleanValue(account.HomeDir) || !filepath.IsAbs(account.HomeDir) {
		return fmt.Errorf("receiver must be an existing non-root account with a valid home directory")
	}
	if installed != nil && (account.Uid != installed.UID || account.Gid != installed.GID || account.HomeDir != installed.Home) {
		return fmt.Errorf("service account changed; uninstall and reinstall explicitly")
	}
	if err := servicePath(account.HomeDir); err != nil {
		return err
	}
	directory, err = canonical(directory)
	if err != nil {
		return err
	}
	if err := servicePath(directory); err != nil {
		return err
	}
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("--data-dir must be an existing directory: %s", directory)
	}
	if err := checkDataOwner(info, uid); err != nil {
		return err
	}
	newInstall := &Installation{ManagedBy: managedBy, User: account.Username, UID: account.Uid, GID: account.Gid, Home: account.HomeDir, DataDir: directory}
	source, err := m.executable()
	if err != nil {
		return err
	}
	binary, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	unit := m.unit(newInstall)
	newInstall.BinaryHash, newInstall.UnitHash = digest(binary), digest(unit)
	manifest, err := json.MarshalIndent(newInstall, "", "  ")
	if err != nil {
		return err
	}
	var before []savedFile
	for _, path := range []string{m.paths.Binary, m.paths.Unit, m.paths.Manifest} {
		s, err := m.snapshot(path)
		if err != nil {
			return err
		}
		if installed == nil && s.exists {
			return fmt.Errorf("refusing existing foreign installation file %s", path)
		}
		before = append(before, s)
	}
	// A unit supplied by a distribution (outside our target path) is foreign too.
	stateOutput, err := m.run(ctx, false, "systemctl", "show", UnitName, "--property=LoadState", "--property=FragmentPath", "--property=UnitFileState")
	if err != nil {
		return err
	}
	wasEnabled := false
	for _, line := range strings.Split(stateOutput, "\n") {
		if line == "UnitFileState=enabled" {
			wasEnabled = true
		}
		if strings.HasPrefix(line, "FragmentPath=") {
			fragment := strings.TrimPrefix(line, "FragmentPath=")
			if fragment != "" && (installed == nil || fragment != m.paths.Unit) {
				return fmt.Errorf("refusing foreign systemd unit %s", fragment)
			}
		}
	}
	rollback := func(cause error) error {
		if recovery := m.rollback(before, wasEnabled); recovery != nil {
			return fmt.Errorf("installation failed: %w; rollback incomplete: %v; inspect service status", cause, recovery)
		}
		return fmt.Errorf("installation failed; previous files and autostart restored: %w", cause)
	}
	for n, data := range [][]byte{binary, unit, manifest} {
		mode := os.FileMode(0644)
		if n == 0 {
			mode = 0755
		}
		if err := m.write(before[n].path, data, mode); err != nil {
			return rollback(err)
		}
	}
	if _, err = m.run(ctx, false, "systemctl", "daemon-reload"); err != nil {
		return rollback(err)
	}
	if _, err = m.run(ctx, false, "systemctl", "enable", UnitName); err != nil {
		return rollback(err)
	}
	return nil
}

func (m *Manager) Uninstall(ctx context.Context) error {
	lock, err := m.mutation()
	if err != nil {
		return err
	}
	defer lock.Close()
	installed, err := m.Read()
	if err != nil || installed == nil {
		return err
	}
	if err = m.verifyFiles(installed); err != nil {
		return err
	}
	state, err := m.state(ctx, installed)
	if err != nil {
		return err
	}
	// A missing unit can be a previous interrupted removal. An already loaded
	// active process must still be stopped even if its file has disappeared.
	if state.Active != "inactive" && state.Active != "missing-unit" {
		if _, err = m.run(ctx, false, "systemctl", "stop", UnitName); err != nil {
			return err
		}
	}
	if _, statErr := os.Lstat(m.paths.Unit); statErr == nil || state.Enabled {
		if _, err = m.run(ctx, false, "systemctl", "disable", UnitName); err != nil {
			return err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	// Keep the manifest until last so interrupted removal can be retried.
	for _, path := range []string{m.paths.Unit, m.paths.Binary} {
		if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if _, err = m.run(ctx, false, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	return os.Remove(m.paths.Manifest)
}
