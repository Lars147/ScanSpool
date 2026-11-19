//go:build !windows

package output

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"scanspool/internal/config"
)

func TestLocalOutputRespectsUmask(t *testing.T) {
	const key = "SCANSPOOL_TEST_UMASK_DIR"
	dir := os.Getenv(key)
	if dir == "" {
		command := exec.Command(os.Args[0], "-test.run=^TestLocalOutputRespectsUmask$")
		command.Env = append(os.Environ(), key+"="+t.TempDir())
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("umask subprocess: %v\n%s", err, output)
		}
		return
	}
	// Process-local: do not change the umask of unrelated tests or goroutines.
	previous := syscall.Umask(0027)
	defer syscall.Umask(previous)
	target := config.Destination{Kind: config.KindLocal, LocalDirectory: filepath.Join(dir, "finished", "scans")}
	name, err := Enqueue(dir, target, []byte("%PDF permissions"))
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{
		queueDir(dir):                              0700,
		filepath.Join(queueDir(dir), name):         0600,
		filepath.Join(queueDir(dir), name+".json"): 0600,
	} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatal("internal permissions changed", path, err)
		}
	}
	jobs, err := Pending(dir)
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	if err := Send(context.Background(), dir, target, jobs[0]); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{
		filepath.Join(dir, "finished"):             0750,
		target.LocalDirectory:                      0750,
		filepath.Join(target.LocalDirectory, name): 0640,
	} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatal("output ignored umask", path, err)
		}
	}
}
