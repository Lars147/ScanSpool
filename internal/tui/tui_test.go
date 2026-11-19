package tui

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"scanspool/internal/config"
	"scanspool/internal/service"
	"scanspool/internal/setup"
)

func TestHeaderDistinguishesReceiverAndTUIVersions(t *testing.T) {
	m := &model{mode: "home", version: "0.1.0", width: 60, height: 18}
	for _, receiver := range []string{"", "dev", "0.1.0"} {
		m.status = service.Status{Running: true, Version: receiver}
		content := m.View().Content
		if !strings.Contains(content, "ScanSpool 0.1.0 · home") {
			t.Fatal("TUI version missing from header")
		}
		if strings.Contains(content, "Receiver version:") != (receiver == "dev") {
			t.Fatalf("unexpected receiver version display for %q: %s", receiver, content)
		}
	}
}

func TestHomeErrorRemainsVisibleDespiteStartMessage(t *testing.T) {
	m := &model{mode: "home", width: 60, height: 18, cursor: len(menu) - 1, message: "Scan started. Check Scanner status for progress and errors.", status: service.Status{
		Configured: true, Online: true, OutputKind: config.KindLocal, OutputReady: true, OutputDirectory: "/volume1/paperless", Pending: 1,
		Error: "scan.pdf: permission denied", ErrorHint: "Permission denied. Check file and folder access rights.",
	}}
	content := m.View().Content
	for _, expected := range []string{"Permission denied", "1 PDF(s) in outbox", "> Quit", "Arrows/Tab"} {
		if !strings.Contains(content, expected) {
			t.Fatalf("missing %q in %s", expected, content)
		}
	}
	m.status.Error, m.status.ErrorHint = "", ""
	if strings.Contains(m.View().Content, "Permission denied") {
		t.Fatal("resolved error banner remained")
	}
}

func TestPermissionDiagnosticWrapsInFormAndStatus(t *testing.T) {
	detail := "cannot use local output folder /volume1/" + strings.Repeat("long-folder-name/", 5) + ": permission denied"
	for _, mode := range []string{"local", "status"} {
		m := &model{mode: mode, width: 60, height: 24}
		if mode == "local" {
			m.message = detail
		} else {
			m.status.Error = detail
			m.status.ErrorHint = "Permission denied. Check file and folder access rights."
		}
		content := m.View().Content
		if !strings.Contains(strings.ReplaceAll(content, "\n", ""), detail) {
			t.Fatalf("diagnostic truncated in %s: %s", mode, content)
		}
	}
}

func TestRepairCommandPreservesShellLineContinuationsInStatus(t *testing.T) {
	hint := "Permission denied.\nRun on receiver as administrator:\nsudo setfacl -m 'u:2042:rwx' -- \\\n  '/volume1/scans'\nThen retry queued deliveries."
	m := &model{mode: "status", width: 80, height: 24, status: service.Status{Error: "permission denied", ErrorHint: hint}}
	content := m.View().Content
	if !strings.Contains(content, "-- \\\n  '/volume1/scans'") {
		t.Fatal("repair command lost continuation", content)
	}
	m.mode = "home"
	content = m.View().Content
	if !strings.Contains(content, "Repair command: open Scanner status.") || strings.Contains(content, "sudo setfacl") {
		t.Fatal("home did not keep repair notice concise", content)
	}
}

func TestDestinationPickerLocalSaveAndDiscard(t *testing.T) {
	api := &testAPI{}
	m := &model{api: api, ctx: context.Background(), width: 80, height: 24}
	view := config.View{Kind: config.KindFTP, Revision: "revision", LocalDirectory: filepath.Join(t.TempDir(), "scans")}
	m.Update(resultMsg{kind: "settings", value: view})
	if m.mode != "destination" || !strings.Contains(m.View().Content, "Local folder") {
		t.Fatal("missing destination picker")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.mode != "local" || len(m.fields) != 1 || m.fields[0].value != view.LocalDirectory {
		t.Fatal("local form not populated")
	}
	result := m.apply()().(resultMsg)
	if result.err != nil || api.saved.Kind != config.KindLocal || api.saved.LocalDirectory != view.LocalDirectory || api.saved.Revision != view.Revision || api.saved.FTP.Host != "" {
		t.Fatal("local form requires FTP or saves wrong target", api.saved, result.err)
	}
	m.Update(result)
	if m.mode != "home" || m.fields != nil {
		t.Fatal("save retained draft")
	}
	m.openDestination(config.View{Kind: config.KindLocal, LocalDirectory: view.LocalDirectory})
	if m.cursor != 1 {
		t.Fatal("active destination not selected")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m.fields[0].value = "discard this draft"
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.mode != "home" || m.fields != nil || api.saved.LocalDirectory != view.LocalDirectory {
		t.Fatal("escape persisted draft")
	}
	m.openDestination(view)
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.mode != "ftp" || len(m.fields) != 5 {
		t.Fatal("FTP form unavailable")
	}
}

type testAPI struct {
	service.API
	saved     config.Update
	connected setup.Request
	scanCalls int
	scanErr   error
}

func (a *testAPI) Save(_ context.Context, u config.Update) (config.View, error) {
	a.saved = u
	return config.View{}, nil
}
func (a *testAPI) Connect(_ context.Context, r setup.Request) error { a.connected = r; return nil }
func (a *testAPI) StartScan(context.Context) error                  { a.scanCalls++; return a.scanErr }

func TestStartScanMenuAcknowledgesAcceptanceAndShowsBusyErrors(t *testing.T) {
	for _, failure := range []error{nil, service.ErrBusy} {
		api := &testAPI{scanErr: failure}
		m := &model{api: api, ctx: context.Background(), mode: "home"}
		for index, label := range menu {
			if label == "Start scan" {
				m.cursor = index
			}
		}
		_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		if cmd == nil || !m.working {
			t.Fatal("menu did not dispatch scan")
		}
		_, duplicate := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		if duplicate != nil {
			t.Fatal("queued duplicate request while working")
		}
		result := cmd().(resultMsg)
		if !errors.Is(result.err, failure) || api.scanCalls != 1 {
			t.Fatal("wrong scan request", result.err, api.scanCalls)
		}
		m.Update(result)
		if m.working {
			t.Fatal("acceptance kept UI blocked for scan duration")
		}
		if failure != nil {
			if m.message != failure.Error() {
				t.Fatal("scan rejection hidden", m.message)
			}
		} else if !strings.HasPrefix(m.message, "Scan started.") {
			t.Fatal("missing acceptance", m.message)
		}
	}
}

func TestFTPPasswordMaskedAndBlankPreserved(t *testing.T) {
	api := &testAPI{}
	m := &model{api: api, ctx: context.Background(), width: 100, height: 30}
	m.openFTP(config.View{FTP: config.FTP{Host: "ftp.test", User: "scan", Directory: "/consume"}, Revision: "rev", HasPassword: true})
	m.fields[2].value = "synthetic-secret"
	if strings.Contains(m.View().Content, "synthetic-secret") {
		t.Fatal("password rendered")
	}
	m.fields[2].value = ""
	result := m.apply()().(resultMsg)
	if result.err != nil || api.saved.PasswordAction != "keep" || api.saved.Revision != "rev" {
		t.Fatal(api.saved, result.err)
	}
	m.Update(result)
	if m.fields != nil {
		t.Fatal("saved password draft retained")
	}
}
func TestRegistrationRequiresExplicitConfirmation(t *testing.T) {
	api := &testAPI{}
	m := &model{api: api, ctx: context.Background(), mode: "register", selected: setup.Candidate{IP: "127.0.0.1", Serial: "test"}, fields: []field{{value: "connection-secret"}, {value: "false", toggle: true}}}
	if cmd := m.apply(); cmd != nil || m.working {
		t.Fatal("implicit registration")
	}
	m.fields[1].value = "true"
	result := m.apply()().(resultMsg)
	if result.err != nil || !api.connected.ConfirmReplacement || api.connected.Password != "connection-secret" {
		t.Fatal("explicit registration missing", result.err)
	}
}
func TestConflictRetainsDraftAndEscapeDiscards(t *testing.T) {
	m := &model{ctx: context.Background(), mode: "ftp", fields: []field{{value: "draft-secret", secret: true}}, working: true}
	m.Update(resultMsg{kind: "save", err: config.ErrConflict})
	if m.mode != "ftp" || m.fields[0].value != "draft-secret" {
		t.Fatal("conflict discarded draft")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.mode != "home" || m.fields != nil {
		t.Fatal("escape retained draft")
	}
}
func TestQuitDuringOperationWaitsForCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &model{working: true, cancel: cancel}
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd != nil || !m.quit || ctx.Err() == nil {
		t.Fatal("quit skipped cancellation and cleanup")
	}
	_, cmd = m.Update(resultMsg{err: context.Canceled})
	if cmd == nil {
		t.Fatal("did not quit after cleanup")
	}
}
