package persist

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDurabilityFailuresBeforeAndAfterCommit(t *testing.T) {
	failure := errors.New("synthetic storage failure")
	for _, stage := range []string{"file-sync", "rename", "directory-sync"} {
		t.Run(stage, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := WriteFile(path, []byte("old")); err != nil {
				t.Fatal(err)
			}
			ops := operations{(*os.File).Sync, os.Rename, (*os.File).Sync}
			switch stage {
			case "file-sync":
				ops.syncFile = func(*os.File) error { return failure }
			case "rename":
				ops.rename = func(string, string) error { return failure }
			case "directory-sync":
				ops.syncDir = func(*os.File) error { return failure }
			}
			err := writeFile(path, []byte("new"), ops)
			if !errors.Is(err, failure) {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(path)
			expected := "old"
			if stage == "directory-sync" {
				expected = "new"
			}
			if string(data) != expected || Committed(err) != (stage == "directory-sync") {
				t.Fatalf("inconsistent commit: %q %v", data, err)
			}
			info, _ := os.Stat(path)
			if info.Mode().Perm() != 0600 {
				t.Fatal("private mode lost")
			}
			files, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".state-*"))
			if len(files) != 0 {
				t.Fatal("temporary state leaked")
			}
		})
	}
}
