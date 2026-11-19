package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"scanspool/internal/control"
)

type unlock struct{ mutex *sync.Mutex }

func (u unlock) Close() error { u.mutex.Unlock(); return nil }

type fixture struct {
	m                       *Manager
	dir, source             string
	account                 *user.User
	active                  string
	enabled, ready, foreign bool
	fail                    string
	calls                   []string
	startReady              bool
	probeErr                error
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "user", "data")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := canonical(dir)
	if err != nil {
		t.Fatal(err)
	}
	uid := os.Geteuid()
	if uid == 0 {
		uid = 1000
		if err := os.Chown(dir, uid, os.Getegid()); err != nil {
			t.Fatal(err)
		}
	}
	f := &fixture{dir: dir, source: filepath.Join(root, "uploaded"), active: "inactive", startReady: true, account: &user.User{Username: "receiver", Uid: strconv.Itoa(uid), Gid: strconv.Itoa(os.Getegid()), HomeDir: filepath.Join(root, "user")}}
	if err := os.WriteFile(f.source, []byte("binary version 1"), 0755); err != nil {
		t.Fatal(err)
	}
	m := New()
	f.m = m
	m.paths = Paths{filepath.Join(root, "bin", "scanspool"), filepath.Join(root, "units", UnitName), filepath.Join(root, "etc", "service.json"), filepath.Join(root, "install.lock")}
	m.platform = func() error { return nil }
	m.uid = func() int { return 0 }
	m.lookup = func(name string) (*user.User, error) {
		if name != "receiver" {
			return nil, fmt.Errorf("missing user")
		}
		return f.account, nil
	}
	m.executable = func() (string, error) { return f.source, nil }
	m.checkFile = func(string, os.FileInfo) error { return nil }
	m.checkParents = func(string) error { return nil }
	var mutex sync.Mutex
	m.lock = func(string) (io.Closer, error) {
		if !mutex.TryLock() {
			return nil, fmt.Errorf("installation busy")
		}
		return unlock{&mutex}, nil
	}
	m.readyTimeout = 30 * time.Millisecond
	m.probe = func(context.Context, string) error {
		if f.probeErr != nil {
			return f.probeErr
		}
		if !f.ready {
			return control.ErrUnavailable
		}
		return nil
	}
	m.run = func(ctx context.Context, stream bool, name string, args ...string) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		call := name + " " + strings.Join(args, " ")
		f.calls = append(f.calls, call)
		action := args[0]
		if f.fail == action {
			f.fail = ""
			return "", fmt.Errorf("injected %s failure", action)
		}
		if name == "journalctl" {
			return "", nil
		}
		switch action {
		case "show":
			fragment, load := "", "not-found"
			if _, err := os.Stat(m.paths.Unit); err == nil {
				fragment, load = m.paths.Unit, "loaded"
			}
			if f.foreign {
				fragment, load = "/usr/lib/systemd/system/"+UnitName, "loaded"
			}
			enabled := "disabled"
			if f.enabled {
				enabled = "enabled"
			}
			return fmt.Sprintf("LoadState=%s\nFragmentPath=%s\nActiveState=%s\nUnitFileState=%s\n", load, fragment, f.active, enabled), nil
		case "enable":
			f.enabled = true
		case "disable":
			f.enabled = false
		case "start", "restart":
			f.active = "active"
			f.ready = f.startReady
		case "stop":
			f.active = "inactive"
			f.ready = false
		case "daemon-reload":
		default:
			return "", fmt.Errorf("unexpected command %s", call)
		}
		return "", nil
	}
	return f
}

func (f *fixture) install(t *testing.T) {
	t.Helper()
	if err := f.m.Install(context.Background(), "receiver", f.dir); err != nil {
		t.Fatal(err)
	}
}

func TestInstallUpgradeAndUninstallPreserveData(t *testing.T) {
	f := newFixture(t)
	private := filepath.Join(f.dir, "outbox", "scan.pdf")
	if err := os.MkdirAll(filepath.Dir(private), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(private, []byte("queued PDF"), 0600); err != nil {
		t.Fatal(err)
	}
	f.install(t)
	if !f.enabled || f.active != "inactive" {
		t.Fatal("install started service or failed to enable")
	}
	i, err := f.m.Read()
	if err != nil || i == nil {
		t.Fatal(i, err)
	}
	if i.DataDir != f.dir || i.User != "receiver" {
		t.Fatal(i)
	}
	info, err := os.Stat(f.m.paths.Binary)
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatal(info, err)
	}
	if err := f.m.Control(context.Background(), "start"); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if err := f.m.Control(context.Background(), "start"); err != nil {
		t.Fatal(err)
	}
	for _, call := range f.calls {
		if strings.Contains(call, "systemctl start") {
			t.Fatal("repeated start must not start another receiver")
		}
	}
	if err := os.WriteFile(f.source, []byte("binary version 2"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Install(context.Background(), "", ""); err != nil {
		t.Fatal(err)
	}
	if f.active != "active" {
		t.Fatal("upgrade interrupted receiver")
	}
	if err := f.m.Control(context.Background(), "restart"); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Control(context.Background(), "stop"); err != nil {
		t.Fatal(err)
	}
	if !f.enabled {
		t.Fatal("stop disabled autostart")
	}
	if err := f.m.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.enabled {
		t.Fatal("uninstall kept enabled")
	}
	for _, path := range []string{f.m.paths.Binary, f.m.paths.Unit, f.m.paths.Manifest} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("install file retained", path, err)
		}
	}
	data, err := os.ReadFile(private)
	if err != nil || string(data) != "queued PDF" {
		t.Fatal("outbox changed", err)
	}
	if err := f.m.Uninstall(context.Background()); err != nil {
		t.Fatal("uninstall not idempotent", err)
	}
}

func TestDirectorySelectionAndIdentityGuard(t *testing.T) {
	f := newFixture(t)
	f.install(t)
	f.m.uid = func() int { n, _ := strconv.Atoi(f.account.Uid); return n }
	path, managed, err := f.m.ResolveDirectory("", "", "/old/default")
	if err != nil || path != f.dir || !managed {
		t.Fatal(path, managed, err)
	}
	for _, method := range []string{"explicit", "environment"} {
		explicit, environment := "", ""
		if method == "explicit" {
			explicit = "/independent"
		} else {
			environment = "/independent"
		}
		path, managed, err = f.m.ResolveDirectory(explicit, environment, "/old/default")
		if err != nil || path != "/independent" || managed {
			t.Fatal(method, path, managed, err)
		}
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(f.dir, alias); err != nil {
		t.Fatal(err)
	}
	f.m.uid = func() int { return 0 }
	for _, path := range []string{f.dir, alias} {
		if _, _, err = f.m.ResolveDirectory(path, "", "/old/default"); err == nil {
			t.Fatal("wrong user allowed", path)
		}
	}
	path, managed, err = f.m.ResolveDirectory("", "", "/other/default")
	if err != nil || path != "/other/default" || managed {
		t.Fatal("other user's default changed", path, managed, err)
	}
}

func TestInstallRejectsIdentityChangesAndForeignFiles(t *testing.T) {
	f := newFixture(t)
	f.install(t)
	for _, change := range [][2]string{{"other", f.dir}, {"receiver", t.TempDir()}} {
		if err := f.m.Install(context.Background(), change[0], change[1]); err == nil {
			t.Fatal("identity change allowed")
		}
	}
	if err := os.WriteFile(f.m.paths.Unit, []byte("foreign unit"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []func() error{func() error { return f.m.Install(context.Background(), "", "") }, func() error { return f.m.Uninstall(context.Background()) }, func() error { return f.m.Control(context.Background(), "restart") }} {
		if err := operation(); err == nil {
			t.Fatal("foreign file accepted")
		}
	}
	for _, kind := range []string{"binary", "symlink", "distribution-unit"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			if kind == "distribution-unit" {
				f.foreign = true
			} else {
				if err := os.MkdirAll(filepath.Dir(f.m.paths.Binary), 0755); err != nil {
					t.Fatal(err)
				}
				var err error
				if kind == "symlink" {
					err = os.Symlink(f.source, f.m.paths.Binary)
				} else {
					err = os.WriteFile(f.m.paths.Binary, []byte("foreign"), 0755)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := f.m.Install(context.Background(), "receiver", f.dir); err == nil {
				t.Fatal("foreign installation accepted")
			}
		})
	}
}

func TestInstallRollback(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, failure := range []string{"daemon-reload", "enable"} {
			t.Run(fmt.Sprintf("existing=%t/%s", existing, failure), func(t *testing.T) {
				f := newFixture(t)
				if existing {
					f.install(t)
				}
				var before []savedFile
				for _, path := range []string{f.m.paths.Binary, f.m.paths.Unit, f.m.paths.Manifest} {
					s, err := f.m.snapshot(path)
					if err != nil {
						t.Fatal(err)
					}
					before = append(before, s)
				}
				if err := os.WriteFile(f.source, []byte("new version"), 0755); err != nil {
					t.Fatal(err)
				}
				f.fail = failure
				if err := f.m.Install(context.Background(), "receiver", f.dir); err == nil {
					t.Fatal("failure hidden")
				}
				if f.enabled != existing {
					t.Fatal("autostart not restored")
				}
				for _, old := range before {
					s, err := f.m.snapshot(old.path)
					if err != nil || !reflect.DeepEqual(s, old) {
						t.Fatalf("%s not restored: %v", old.path, err)
					}
				}
			})
		}
	}
}

func TestReadinessConflictsAndStopFailures(t *testing.T) {
	f := newFixture(t)
	f.install(t)
	f.ready = true
	if err := f.m.Control(context.Background(), "start"); err == nil || !strings.Contains(err.Error(), "another receiver") {
		t.Fatal(err)
	}
	f.ready = false
	f.startReady = false
	if err := f.m.Control(context.Background(), "start"); err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatal(err)
	}
	f.active = "inactive"
	f.fail = "start"
	if err := f.m.Control(context.Background(), "start"); err == nil {
		t.Fatal("systemctl error hidden")
	}
	f.fail = "stop"
	f.active = "active"
	if err := f.m.Uninstall(context.Background()); err == nil {
		t.Fatal("failed stop ignored")
	}
	if i, err := f.m.Read(); err != nil || i == nil {
		t.Fatal("failed stop removed installation", err)
	}
	f.fail = ""
	f.active = "inactive"
	f.probeErr = fmt.Errorf("corrupt endpoint")
	if err := f.m.Control(context.Background(), "start"); err == nil || !strings.Contains(err.Error(), "cannot check") {
		t.Fatal(err)
	}
}

func TestMutationPermissionsLockAndLogs(t *testing.T) {
	f := newFixture(t)
	f.m.uid = func() int { return 123 }
	if err := f.m.Install(context.Background(), "receiver", f.dir); err == nil || !strings.Contains(err.Error(), "sudo") {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Fatal("unauthorized system call")
	}
	f.m.uid = func() int { return 0 }
	lock, err := f.m.lock("")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.m.Install(context.Background(), "receiver", f.dir); err == nil {
		t.Fatal("concurrent installation allowed")
	}
	lock.Close()
	if err = f.m.Logs(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.calls[len(f.calls)-1], "--lines=100 --no-pager --follow") {
		t.Fatal(f.calls)
	}
}

func TestUnitEscapingAndOldSystemdDirectives(t *testing.T) {
	f := newFixture(t)
	i := &Installation{UID: "1000", GID: "100", Home: "/home/a$b", DataDir: "/mount/a b'$x"}
	unit := string(f.m.unit(i))
	for _, expected := range []string{"Type=simple", "UMask=0022", "TimeoutStopSec=120s", `Environment="HOME=/home/a$b"`, `RequiresMountsFor="/mount/a b'$x"`, `WorkingDirectory=/mount/a b'$x`, `run --data-dir "/mount/a b'$$x"`} {
		if !strings.Contains(unit, expected) {
			t.Errorf("missing %q in\n%s", expected, unit)
		}
	}
	for _, modern := range []string{"Type=exec", "StateDirectory=", "DynamicUser="} {
		if strings.Contains(unit, modern) {
			t.Fatal("unsupported219 directive", modern)
		}
	}
	for _, path := range []string{"/path/%name", "/path/a\"b", `/path/a\b`, "/path/trailing ", "/path/control\n"} {
		if err := servicePath(path); err == nil {
			t.Errorf("unsupported219 path accepted: %q", path)
		}
	}
}

func TestUnrelatedDirectoryDoesNotTraversePrivateServiceHome(t *testing.T) {
	f := newFixture(t)
	f.install(t)
	// An inaccessible or broken service path must not affect another account.
	if err := os.Rename(f.dir, f.dir+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.dir, f.dir); err != nil {
		t.Fatal(err)
	}
	f.m.uid = func() int { return 0 }
	other := t.TempDir()
	if _, managed, err := f.m.ResolveDirectory("", "", other); err != nil || managed {
		t.Fatal("unrelated user touched service path", managed, err)
	}
}

func TestInstallWriteFailureRollback(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for step := 0; step < 3; step++ {
			t.Run(fmt.Sprintf("existing=%t/step=%d", existing, step), func(t *testing.T) {
				f := newFixture(t)
				if existing {
					f.install(t)
				}
				var before []savedFile
				for _, path := range []string{f.m.paths.Binary, f.m.paths.Unit, f.m.paths.Manifest} {
					s, err := f.m.snapshot(path)
					if err != nil {
						t.Fatal(err)
					}
					before = append(before, s)
				}
				f.m.write = func(path string, data []byte, mode os.FileMode) error {
					if path == before[step].path {
						return fmt.Errorf("injected disk failure")
					}
					return atomicFile(path, data, mode)
				}
				if err := f.m.Install(context.Background(), "receiver", f.dir); err == nil {
					t.Fatal("write failure hidden")
				}
				for _, old := range before {
					s, err := f.m.snapshot(old.path)
					if err != nil || !reflect.DeepEqual(s, old) {
						t.Fatal("rollback failed", old.path, err)
					}
				}
				if f.enabled != existing {
					t.Fatal("autostart state lost")
				}
			})
		}
	}
}

func TestUninstallCanResumeAfterMissingUnit(t *testing.T) {
	f := newFixture(t)
	f.install(t)
	f.fail = "daemon-reload"
	if err := f.m.Uninstall(context.Background()); err == nil {
		t.Fatal("reload failure hidden")
	}
	if _, err := f.m.Read(); err != nil {
		t.Fatal(err)
	}
	original := f.m.run
	f.m.run = func(ctx context.Context, stream bool, name string, args ...string) (string, error) {
		if name == "systemctl" && (args[0] == "stop" || args[0] == "disable") {
			return "", fmt.Errorf("unit not loaded")
		}
		return original(ctx, stream, name, args...)
	}
	if err := f.m.Uninstall(context.Background()); err != nil {
		t.Fatal("cannot resume partial uninstall", err)
	}
}
