package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func TestScanCommandRequiresRunningReceiver(t *testing.T) {
	dir := t.TempDir()
	if err := command([]string{"scan", "--data-dir", dir}); err == nil || !strings.Contains(err.Error(), "no running receiver") {
		t.Fatal("missing receiver was not explained", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "service.lock")); !os.IsNotExist(err) {
		t.Fatal("scan command started a second receiver", err)
	}
}

func TestScanCommandUsesExistingAuthenticatedReceiver(t *testing.T) {
	var scans atomic.Int32
	token := strings.Repeat("a", 64)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("unauthenticated status")
		}
		fmt.Fprint(w, `{"running":true}`)
	})
	mux.HandleFunc("POST /v1/scan", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("unauthenticated scan")
		}
		scans.Add(1)
		fmt.Fprint(w, `{}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	dir := t.TempDir()
	data, err := json.Marshal(map[string]any{"address": strings.TrimPrefix(server.URL, "http://"), "token": token, "protocol": 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "control.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := command([]string{"scan", "--data-dir", dir}); err != nil {
		t.Fatal("scan command failed", err)
	}
	if scans.Load() != 1 {
		t.Fatal("wrong scan request count", scans.Load())
	}
}

func TestSetupLogsDoNotCorruptTerminalAndAppendAcrossSessions(t *testing.T) {
	var terminal bytes.Buffer
	original := log.Writer()
	log.SetOutput(&terminal)
	defer log.SetOutput(original)
	dir := filepath.Join(t.TempDir(), "data")
	for _, message := range []string{"pairing accepted", "connected to scanner"} {
		restore, err := setupLogging(dir)
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer restore()
			// Scanner workers log asynchronously while the TUI owns the terminal.
			done := make(chan struct{})
			go func() {
				defer close(done)
				slog.Info(message)
				slog.Warn("connection diagnostic")
				log.Print("standard logger diagnostic")
				slog.Debug("debug-only protocol data")
			}()
			<-done
			if terminal.Len() != 0 {
				t.Fatalf("background logs reached the terminal: %s", terminal.String())
			}
		}()
	}
	path := filepath.Join(dir, "scanspool.log")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []string{"pairing accepted", "connected to scanner", "connection diagnostic", "standard logger diagnostic"} {
		if !bytes.Contains(data, []byte(message)) {
			t.Fatalf("missing diagnostic %q in %s", message, data)
		}
	}
	if bytes.Contains(data, []byte("debug-only protocol data")) {
		t.Fatal("setup logging enabled protocol debug output")
	}
	slog.Info("headless logging restored")
	if !strings.Contains(terminal.String(), "headless logging restored") {
		t.Fatal("logging was not restored after setup")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("setup log is not private", info.Mode())
	}
}

func TestSetupLoggingFailsBeforeTakingOverTerminal(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "scanspool.log"), 0700); err != nil {
		t.Fatal(err)
	}
	previous := log.Writer()
	if restore, err := setupLogging(dir); err == nil || restore != nil {
		if restore != nil {
			restore()
		}
		t.Fatal("accepted unusable log path")
	}
	if log.Writer() != previous {
		t.Fatal("failed setup changed the logging destination")
	}
}
