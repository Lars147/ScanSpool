package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRejectedLocalProbePreservesSavedConfiguration(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Save(Update{Revision: store.View().Revision, FTP: FTP{Host: "ftp.test", User: "scan", Directory: "/consume"}})
	if err != nil {
		t.Fatal(err)
	}
	before := store.Get()
	saved, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(dir, "not-a-folder")
	if err := os.WriteFile(blocked, []byte("existing file"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(Update{Revision: store.View().Revision, Kind: KindLocal, LocalDirectory: filepath.Join(blocked, "scans")}); err == nil {
		t.Fatal("unusable folder saved")
	}
	after, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil || !bytes.Equal(saved, after) || store.Get() != before {
		t.Fatal("failed validation changed settings", err)
	}
	missing := filepath.Join(dir, "stale-request")
	if _, err := store.Save(Update{Revision: "stale", Kind: KindLocal, LocalDirectory: missing}); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale request created output folder", err)
	}
}

func TestLocalSaveRejectsActualPermissionDenial(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires Unix permissions and unprivileged receiver")
	}
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	readonly := filepath.Join(dir, "readonly")
	if err := os.Mkdir(readonly, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(readonly, 0700)
	before := store.View()
	_, err = store.Save(Update{Revision: before.Revision, Kind: KindLocal, LocalDirectory: readonly})
	if !errors.Is(err, os.ErrPermission) {
		t.Fatal("unwritable output accepted", err)
	}
	if store.View() != before {
		t.Fatal("permission failure changed settings")
	}
	entries, err := os.ReadDir(readonly)
	if err != nil || len(entries) != 0 {
		t.Fatal("permission probe left files", entries, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "settings.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed settings persisted", err)
	}
}

func TestLocalProbeRejectsOutboxSymlinkBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	outbox := filepath.Join(dir, "outbox")
	if err := os.Mkdir(outbox, 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(outbox, "existing.pdf")
	if err := os.WriteFile(source, []byte("%PDF keep"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(outbox, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := store.Save(Update{Revision: store.View().Revision, Kind: KindLocal, LocalDirectory: alias}); err == nil {
		t.Fatal("outbox alias accepted")
	}
	data, err := os.ReadFile(source)
	if err != nil || string(data) != "%PDF keep" {
		t.Fatal("probe changed outbox PDF", err)
	}
	entries, err := os.ReadDir(outbox)
	if err != nil || len(entries) != 1 {
		t.Fatal("probe wrote inside outbox", entries, err)
	}
}
