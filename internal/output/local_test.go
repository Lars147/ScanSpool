package output

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"scanspool/internal/config"
	"scanspool/internal/persist"
)

func TestLocalQueueCreatesDirectoriesAndPublishesCompletePDF(t *testing.T) {
	dir := t.TempDir()
	target := config.Destination{Kind: config.KindLocal, LocalDirectory: filepath.Join(dir, "finished", "scans"), FTP: config.FTP{Password: "unused-secret", Host: "unused-host"}}
	data := []byte("%PDF complete document")
	name, err := Enqueue(dir, target, data)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := os.ReadFile(filepath.Join(queueDir(dir), name+".json"))
	if err != nil || bytes.Contains(metadata, []byte("unused-")) {
		t.Fatal("inactive FTP settings leaked to local job", err)
	}
	jobs, err := Pending(dir)
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	if err := Send(context.Background(), dir, target, jobs[0]); err != nil {
		t.Fatal(err)
	}
	pdf, err := os.ReadFile(filepath.Join(target.LocalDirectory, name))
	if err != nil || !bytes.Equal(pdf, data) {
		t.Fatal("local PDF differs", err)
	}
	entries, err := os.ReadDir(target.LocalDirectory)
	if err != nil || len(entries) != 1 || entries[0].Name() != name {
		t.Fatal("temporary file leaked", entries, err)
	}
	jobs, err = Pending(dir)
	if err != nil || len(jobs) != 0 {
		t.Fatal("delivered job remained queued", err)
	}
	if _, err := os.Stat(filepath.Join(queueDir(dir), name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("delivered source PDF retained", err)
	}
}

func TestLocalFailureAndTargetChangeRetainQueuedPDF(t *testing.T) {
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	target := config.Destination{Kind: config.KindLocal, LocalDirectory: filepath.Join(blocked, "scans")}
	name, err := Enqueue(dir, target, []byte("%PDF complete"))
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := Pending(dir)
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	if err := Send(context.Background(), dir, target, jobs[0]); err == nil {
		t.Fatal("blocked directory reported success")
	}
	wrong := target
	wrong.LocalDirectory = filepath.Join(dir, "other")
	if err := Send(context.Background(), dir, wrong, jobs[0]); err == nil {
		t.Fatal("rerouted queued document")
	}
	for _, suffix := range []string{"", ".json"} {
		if _, err := os.Stat(filepath.Join(queueDir(dir), name+suffix)); err != nil {
			t.Fatal("lost recoverable job", err)
		}
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if err := Send(context.Background(), dir, target, jobs[0]); err != nil {
		t.Fatal("retry failed", err)
	}
}

func TestLocalRejectsOutboxAliases(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(map[bool]string{false: "outbox", true: "outbox-child"}[nested], func(t *testing.T) {
			dir := t.TempDir()
			alias := filepath.Join(dir, "alias")
			target := config.Destination{Kind: config.KindLocal, LocalDirectory: alias}
			if nested {
				target.LocalDirectory = filepath.Join(alias, "child")
			}
			name, err := Enqueue(dir, target, []byte("%PDF keep me"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(queueDir(dir), alias); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			jobs, err := Pending(dir)
			if err != nil || len(jobs) != 1 {
				t.Fatal(jobs, err)
			}
			if err := Send(context.Background(), dir, target, jobs[0]); err == nil {
				t.Fatal("accepted output inside outbox")
			}
			data, err := os.ReadFile(filepath.Join(queueDir(dir), name))
			if err != nil || string(data) != "%PDF keep me" {
				t.Fatal("outbox source damaged", err)
			}
			if _, err := os.Stat(filepath.Join(queueDir(dir), name+".json")); err != nil {
				t.Fatal("lost recoverable job metadata", err)
			}
		})
	}
}

func localTestOperations() localOperations {
	return localOperations{(*os.File).Sync, (*os.Root).Link, syncOutputDirectory}
}

func TestLocalPublicationFailuresAndImmediateConsumer(t *testing.T) {
	failure := errors.New("storage failure")
	for _, stage := range []string{"file-sync", "publish", "directory-sync", "immediate-consumer"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(queueDir(dir), 0700); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(dir, "finished")
			ops := localTestOperations()
			switch stage {
			case "file-sync":
				ops.syncFile = func(*os.File) error { return failure }
			case "publish":
				ops.publish = func(*os.Root, string, string) error { return failure }
			case "directory-sync":
				ops.syncDir = func(*os.File) error { return failure }
			case "immediate-consumer":
				ops.publish = func(root *os.Root, temporary, name string) error {
					if err := root.Link(temporary, name); err != nil {
						return err
					}
					data, err := root.ReadFile(name)
					if err != nil || string(data) != "%PDF finished" {
						t.Fatal("consumer received incomplete PDF", err)
					}
					return root.Remove(name)
				}
			}
			err := writeLocal(context.Background(), dir, destination, "scan.pdf", strings.NewReader("%PDF finished"), ops)
			if stage == "immediate-consumer" {
				if err != nil {
					t.Fatal("consumer removal became delivery failure", err)
				}
			} else if !errors.Is(err, failure) || persist.Committed(err) != (stage == "directory-sync") {
				t.Fatal("wrong commit result", err)
			}
			entries, err := os.ReadDir(destination)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if stage == "directory-sync" {
				want = 1
				// Retrying an already published job is safe and leaves one PDF.
				if err := Local(context.Background(), dir, destination, "scan.pdf", strings.NewReader("%PDF finished")); err != nil {
					t.Fatal("retry after uncertain commit failed", err)
				}
			}
			if len(entries) != want {
				t.Fatal("partial output or temporary file leaked", entries)
			}
		})
	}
}

func TestLocalNeverOverwritesCollision(t *testing.T) {
	for _, existing := range []string{"%PDF same", "%PDF different"} {
		t.Run(existing, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(queueDir(dir), 0700); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(dir, "finished")
			ops := localTestOperations()
			// Create the competing file at publication time, not before staging.
			ops.publish = func(root *os.Root, temporary, name string) error {
				if err := root.WriteFile(name, []byte(existing), 0600); err != nil {
					return err
				}
				return root.Link(temporary, name)
			}
			err := writeLocal(context.Background(), dir, destination, "scan.pdf", strings.NewReader("%PDF same"), ops)
			if (err == nil) != (existing == "%PDF same") {
				t.Fatal("incorrect collision result", err)
			}
			data, err := os.ReadFile(filepath.Join(destination, "scan.pdf"))
			if err != nil || string(data) != existing {
				t.Fatal("overwrote existing destination", err)
			}
		})
	}
}

type cancelReader struct {
	cancel context.CancelFunc
}

func (r cancelReader) Read(p []byte) (int, error) {
	r.cancel()
	return copy(p, "%PDF interrupted"), nil
}

func TestLocalCancellationNeverPublishes(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(queueDir(dir), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	destination := filepath.Join(dir, "finished")
	err := Local(ctx, dir, destination, "scan.pdf", cancelReader{cancel})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(destination)
	if err != nil || len(entries) != 0 {
		t.Fatal("cancellation published output", entries, err)
	}
}

func TestLegacyQueuedFTPTargetStillDelivers(t *testing.T) {
	target, server := ftpServer(t, false)
	dir := t.TempDir()
	name, err := Enqueue(dir, config.Destination{FTP: target}, []byte("%PDF legacy"))
	if err != nil {
		t.Fatal(err)
	}
	legacy := struct {
		Name   string     `json:"name"`
		Target config.FTP `json:"target"`
	}{Name: name, Target: target}
	legacy.Target.Password = ""
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(queueDir(dir), name+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	jobs, err := Pending(dir)
	if err != nil || len(jobs) != 1 || jobs[0].Target.OutputKind() != config.KindFTP {
		t.Fatal("legacy job rejected", err)
	}
	if err := Send(context.Background(), dir, config.Destination{Kind: config.KindFTP, FTP: target}, jobs[0]); err != nil {
		t.Fatal("legacy delivery failed", err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if string(server.files[name]) != "%PDF legacy" {
		t.Fatal("legacy PDF lost")
	}
}
