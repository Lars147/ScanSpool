package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyFTPSettingsAndLocalSwitchPreserveCredentials(t *testing.T) {
	dir := t.TempDir()
	legacy := FTP{Host: "ftp.test", User: "scan", Password: "synthetic-secret", Directory: "/consume"}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil || store.Get().OutputKind() != KindFTP || store.Get().Validate() != nil {
		t.Fatal("legacy settings rejected", err)
	}
	initial := store.View()
	localDir := filepath.Join(dir, "scans") + string(filepath.Separator) + ".." + string(filepath.Separator) + "finished"
	local, err := store.Save(Update{Revision: initial.Revision, Kind: KindLocal, LocalDirectory: localDir, FTP: FTP{Host: "ignored", Password: "ignored"}})
	if err != nil || local.Kind != KindLocal || local.LocalDirectory != filepath.Clean(localDir) {
		t.Fatal(local, err)
	}
	if store.Get().FTP != legacy || local.FTP.Password != "" || !local.HasPassword || local.Revision == initial.Revision {
		t.Fatal("local save lost credentials, leaked secret or failed to revise")
	}
	if entries, err := os.ReadDir(local.LocalDirectory); err != nil || len(entries) != 0 {
		t.Fatal("output directory was not checked or contains probe leftovers", err)
	}
	if _, err := store.Save(Update{Revision: initial.Revision, Kind: KindLocal, LocalDirectory: filepath.Join(dir, "other")}); !errors.Is(err, ErrConflict) {
		t.Fatal("stale local edit accepted", err)
	}
	reloaded, err := Open(dir)
	if err != nil || reloaded.Get() != store.Get() {
		t.Fatal("local settings did not survive restart", err)
	}
	ftp, err := reloaded.Save(Update{Revision: local.Revision, Kind: KindFTP, FTP: local.FTP, PasswordAction: "keep"})
	if err != nil || ftp.Kind != KindFTP || reloaded.Get().FTP != legacy || ftp.LocalDirectory != local.LocalDirectory {
		t.Fatal("return to FTP lost saved configuration", err)
	}
}

func TestLocalConfigurationWithoutFTPAndInvalidPaths(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"", "relative", filepath.Join(dir, "outbox"), filepath.Join(dir, "outbox", "nested"), filepath.Join(dir, "bad\npath")} {
		if _, err := store.Save(Update{Revision: store.View().Revision, Kind: KindLocal, LocalDirectory: directory}); err == nil {
			t.Fatalf("invalid local path accepted: %q", directory)
		}
	}
	saved, err := store.Save(Update{Revision: store.View().Revision, Kind: KindLocal, LocalDirectory: filepath.Join(dir, "outbox-other")})
	if err != nil || store.Get().Validate() != nil || saved.FTP.Host != "" {
		t.Fatal("local mode requires FTP or rejects outbox sibling", err)
	}
	if _, err := store.Save(Update{Revision: saved.Revision, Kind: "unsupported"}); err == nil {
		t.Fatal("unknown destination accepted")
	}
}

func TestSecretRedactionAndOptimisticSave(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	first := store.View()
	update := Update{Revision: first.Revision, FTP: FTP{Host: "127.0.0.1:2121", User: "scan", Password: "synthetic-secret", Directory: "/consume"}, PasswordAction: "set"}
	saved, err := store.Save(update)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.HasPassword || saved.FTP.Password != "" {
		t.Fatal("secret exposed in view")
	}
	if _, err := store.Save(update); !errors.Is(err, ErrConflict) {
		t.Fatal("stale save accepted", err)
	}
	_, err = store.Save(Update{Revision: saved.Revision, FTP: saved.FTP, PasswordAction: "keep"})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := Open(dir)
	if err != nil || loaded.Get().Password != "synthetic-secret" {
		t.Fatal("password lost", err)
	}
	info, err := os.Stat(filepath.Join(dir, "settings.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("private settings mode", err)
	}
}
func TestFTPRejectsInvalidCommandsAndDestinations(t *testing.T) {
	for _, target := range []FTP{{Host: "ftp://server", User: "scan", Directory: "/"}, {Host: "server:abc", User: "scan", Directory: "/"}, {Host: "server", User: "scan\r\nQUIT", Directory: "/"}, {Host: "server", User: "scan", Directory: "relative"}} {
		if target.Validate() == nil {
			t.Fatal("invalid FTP settings accepted", strings.ReplaceAll(target.Host, "\n", ""))
		}
	}
}
