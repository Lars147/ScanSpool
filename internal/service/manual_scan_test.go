package service

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"scanspool/internal/config"
	"scanspool/internal/output"
	"scanspool/internal/scanner"
	"scanspool/internal/setup"
	"scanspool/internal/vens"
)

func TestChangedOutputPermissionsPreventFeedingAndKeepQueuedPDF(t *testing.T) {
	if goruntime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires Unix permissions and unprivileged receiver")
	}
	app, sc, _ := manualScanApp(t)
	target := app.settings.Get()
	name, err := output.Enqueue(app.dir, target, []byte("%PDF queued complete document"))
	if err != nil {
		t.Fatal(err)
	}
	var scans atomic.Int32
	sc.scan = func(vens.ScanConfig, func(vens.Page)) ([]vens.Page, error) { scans.Add(1); return nil, nil }
	if err := os.Chmod(target.LocalDirectory, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(target.LocalDirectory, 0700)
	if err := app.StartScan(context.Background()); !errors.Is(err, os.ErrPermission) {
		t.Fatal("scan accepted inaccessible destination", err)
	}
	state, err := app.Status(context.Background())
	if err != nil || state.Busy || state.Pending != 1 || !strings.Contains(state.ErrorHint, "Permission denied") || scans.Load() != 0 {
		t.Fatal("permission failure hidden or paper feed started", state, scans.Load(), err)
	}
	if _, err := os.Stat(filepath.Join(app.dir, "outbox", name)); err != nil {
		t.Fatal("queued PDF lost", err)
	}
	if err := os.Chmod(target.LocalDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := app.Retry(context.Background()); err != nil {
		t.Fatal("retry after permission repair failed", err)
	}
	state, err = app.Status(context.Background())
	if err != nil || state.Pending != 0 || state.Error != "" || state.ErrorHint != "" {
		t.Fatal("resolved error remained active", state, err)
	}
	if data, err := os.ReadFile(filepath.Join(target.LocalDirectory, name)); err != nil || string(data) != "%PDF queued complete document" {
		t.Fatal("recovered PDF differs", err)
	}
}

type manualScanner struct {
	*scanner.Scanner
	online bool
	scan   func(vens.ScanConfig, func(vens.Page)) ([]vens.Page, error)
}

func (s *manualScanner) Online() bool { return s.online }
func (s *manualScanner) Scan(cfg vens.ScanConfig, onPage func(vens.Page)) ([]vens.Page, error) {
	return s.scan(cfg, onPage)
}

func manualScanApp(t *testing.T) (*App, *manualScanner, vens.Page) {
	t.Helper()
	dir := t.TempDir()
	app, err := New(context.Background(), dir, "test", "run")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	info := vens.DeviceInfo{Name: "ScanSnap iX1300", Serial: "TEST000001", InterfaceVersion: 16}
	template, err := vens.PCReserveTemplate(info, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.Save(setup.Connection{Host: "127.0.0.1", Model: info.Name, Serial: info.Serial, Mode: "pc", Template: template}); err != nil {
		t.Fatal(err)
	}
	_, err = app.settings.Save(config.Update{Revision: app.settings.View().Revision, Kind: config.KindLocal, LocalDirectory: filepath.Join(dir, "finished")})
	if err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	if err := jpeg.Encode(&data, image.NewRGBA(image.Rect(0, 0, 20, 20)), nil); err != nil {
		t.Fatal(err)
	}
	page := vens.Page{JPEG: data.Bytes()}
	sc := &manualScanner{Scanner: scanner.New("127.0.0.1", 0, 0, ""), online: true,
		scan: func(vens.ScanConfig, func(vens.Page)) ([]vens.Page, error) { return []vens.Page{page}, nil }}
	app.current = &runtime{sc: sc, button: scanner.NewButtonListener("127.0.0.1", nil), cancel: func() {}}
	app.running = true
	return app, sc, page
}

func TestManualScanUsesSharedPipelineAndOutlivesRequest(t *testing.T) {
	app, sc, page := manualScanApp(t)
	entered, finish := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(finish) }) }
	t.Cleanup(release)
	var calls atomic.Int32
	sc.scan = func(cfg vens.ScanConfig, onPage func(vens.Page)) ([]vens.Page, error) {
		onPage(page)
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-finish
		if cfg != vens.PCReferenceConfig() {
			return nil, errors.New("changed scan profile")
		}
		return []vens.Page{page}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := app.StartScan(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("accepted scan did not start")
	}
	cancel() // An HTTP request/CLI may end immediately after acknowledgement.
	if !app.guard.Busy() {
		t.Fatal("request cancellation released the active scan")
	}
	progress, err := app.Status(context.Background())
	if err != nil || progress.LastScan == "" || progress.LastPages != 1 || !progress.Busy {
		t.Fatal("live page progress missing", progress, err)
	}
	if err := app.StartScan(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatal("parallel scan accepted", err)
	}
	app.startScan(sc) // A physical button press must use the same lifetime guard.
	if err := app.Retry(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatal("delivery retry entered active scan", err)
	}
	release()
	app.workers.Wait()
	if calls.Load() != 1 {
		t.Fatal("duplicate scan", calls.Load())
	}
	state, err := app.Status(context.Background())
	if err != nil || state.Busy || state.Error != "" || state.Pending != 0 || state.LastPages != 1 || state.LastFile == "" {
		t.Fatal("manual scan did not finish delivery", state, err)
	}
	pdf, err := os.ReadFile(filepath.Join(app.settings.Get().LocalDirectory, state.LastFile))
	if err != nil || !bytes.HasPrefix(pdf, []byte("%PDF-")) || !bytes.Contains(pdf, page.JPEG) {
		t.Fatal("wrong manual PDF", err)
	}
}

func TestManualScanRejectsUnavailableStatesWithoutOwningGuard(t *testing.T) {
	for _, scenario := range []string{"unconfigured", "recovery", "not-running", "no-runtime", "offline", "invalid-output", "cancelled-request", "closed-receiver"} {
		t.Run(scenario, func(t *testing.T) {
			app, sc, _ := manualScanApp(t)
			ctx := context.Background()
			want := ""
			switch scenario {
			case "unconfigured":
				if err := app.store.Clear(); err != nil {
					t.Fatal(err)
				}
				want = "configure a scanner"
			case "recovery":
				if err := os.WriteFile(filepath.Join(app.dir, "pending-registration.json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
				want = "recover"
			case "not-running":
				app.running = false
				want = "start the receiver"
			case "no-runtime":
				app.current = nil
				want = "start the receiver"
			case "offline":
				sc.online = false
				want = "offline"
			case "invalid-output":
				app.settings, _ = config.Open(t.TempDir())
				want = "FTP host"
			case "cancelled-request":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = "context canceled"
			case "closed-receiver":
				app.cancel()
				want = "context canceled"
			}
			if err := app.StartScan(ctx); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatal("wrong rejection", err)
			}
			if app.guard.Busy() {
				t.Fatal("rejected scan leaked the operation lease")
			}
		})
	}
}

func TestManualScanFailureNeverPublishesPartialPDF(t *testing.T) {
	app, sc, page := manualScanApp(t)
	sc.scan = func(vens.ScanConfig, func(vens.Page)) ([]vens.Page, error) {
		return []vens.Page{page}, errors.New("paper jam")
	}
	if err := app.StartScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	app.workers.Wait()
	state, err := app.Status(context.Background())
	if err != nil || state.Error != "paper jam" || state.Busy || state.LastFile != "" {
		t.Fatal(state, err)
	}
	jobs, err := output.Pending(app.dir)
	if err != nil || len(jobs) != 0 {
		t.Fatal("partial scan queued", jobs, err)
	}
	if entries, err := os.ReadDir(app.settings.Get().LocalDirectory); err != nil || len(entries) != 0 {
		t.Fatal("partial output published", entries, err)
	}
}

func TestReceiverShutdownWaitsForManualScanCleanup(t *testing.T) {
	app, sc, _ := manualScanApp(t)
	entered, cancelled, cleaned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	finishCleanup := func() { once.Do(func() { close(cleaned) }) }
	t.Cleanup(finishCleanup)
	sc.scan = func(vens.ScanConfig, func(vens.Page)) ([]vens.Page, error) {
		close(entered)
		<-app.ctx.Done()
		close(cancelled)
		<-cleaned
		return nil, app.ctx.Err()
	}
	if err := app.StartScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("scan did not start")
	}
	closed := make(chan struct{})
	go func() { app.Close(); close(closed) }()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("receiver did not cancel scan")
	}
	select {
	case <-closed:
		t.Fatal("receiver returned before cleanup")
	default:
	}
	if !app.guard.Busy() {
		t.Fatal("cleanup lost the scan lease")
	}
	finishCleanup()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("receiver failed to finish cleanup")
	}
}

func TestSavedProfileAppliedToBothScanTriggers(t *testing.T) {
	app, sc, page := manualScanApp(t)
	ctx := context.Background()
	view, err := app.Profile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	view.Profile.ColorMode = vens.ColorGray
	view.Profile.Duplex = false
	view.Profile.BlankPageRemoval = true
	if _, err := app.SaveProfile(ctx, view); err != nil {
		t.Fatal(err)
	}
	scans := make(chan vens.ScanConfig, 2)
	sc.scan = func(cfg vens.ScanConfig, _ func(vens.Page)) ([]vens.Page, error) {
		scans <- cfg
		return []vens.Page{page}, nil
	}
	for _, trigger := range []func() error{
		func() error { return app.StartScan(ctx) },
		func() error { app.startScan(sc); return nil },
	} {
		if err := trigger(); err != nil {
			t.Fatal(err)
		}
		app.workers.Wait()
		select {
		case got := <-scans:
			want := view.Profile
			want.BlankPageRemoval = false // Filter on the receiver after transfer.
			if got != want {
				t.Fatalf("scanner got %+v, want %+v", got, want)
			}
		default:
			t.Fatal("scan did not start")
		}
		status, err := app.Status(ctx)
		if err != nil || status.Error != "" || status.LastFile == "" || status.LastPages != 1 {
			t.Fatal("scan did not deliver its marked page", status, err)
		}
	}
}
