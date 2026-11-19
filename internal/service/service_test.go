package service

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"

	"scanspool/internal/config"
	"scanspool/internal/output"
	"scanspool/internal/vens"
)

type completeScan struct{ page vens.Page }

func (s completeScan) Scan(vens.ScanConfig, func(vens.Page)) ([]vens.Page, error) {
	return []vens.Page{s.page}, nil
}

func TestLocalScanWorksWithoutFTP(t *testing.T) {
	dir := t.TempDir()
	app, err := New(context.Background(), dir, "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	destination := filepath.Join(dir, "finished", "scans")
	_, err = app.Save(context.Background(), config.Update{Revision: app.settings.View().Revision, Kind: config.KindLocal, LocalDirectory: destination})
	if err != nil {
		t.Fatal(err)
	}
	var jpegData bytes.Buffer
	if err := jpeg.Encode(&jpegData, image.NewRGBA(image.Rect(0, 0, 20, 20)), nil); err != nil {
		t.Fatal(err)
	}
	app.startScan(completeScan{vens.Page{JPEG: jpegData.Bytes()}})
	app.workers.Wait()
	state, err := app.Status(context.Background())
	if err != nil || state.Error != "" || state.Pending != 0 || state.LastPages != 1 || !state.OutputReady || state.FTPReady || state.OutputKind != config.KindLocal || state.OutputDirectory != destination {
		t.Fatal("local scan/status failed", state, err)
	}
	pdf, err := os.ReadFile(filepath.Join(destination, state.LastFile))
	if err != nil || !bytes.HasPrefix(pdf, []byte("%PDF-")) || !bytes.Contains(pdf, jpegData.Bytes()) {
		t.Fatal("local PDF missing or invalid", err)
	}
}

func TestForeignAndFailedJobsDoNotBlockMatchingDeliveries(t *testing.T) {
	dir := t.TempDir()
	app, err := New(context.Background(), dir, "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	foreign := config.Destination{FTP: config.FTP{Host: "ftp.test", User: "scan", Directory: "/consume"}}
	if _, err := output.Enqueue(dir, foreign, []byte("%PDF old FTP")); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(dir, "finished")
	_, err = app.Save(context.Background(), config.Update{Revision: app.settings.View().Revision, Kind: config.KindLocal, LocalDirectory: destination})
	if err != nil {
		t.Fatal(err)
	}
	target := app.settings.Get()
	for i := 0; i < 2; i++ {
		if _, err := output.Enqueue(dir, target, []byte("%PDF local")); err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := output.Pending(dir)
	if err != nil {
		t.Fatal(err)
	}
	var matching []output.Job
	for _, job := range jobs {
		if target.SameTarget(job.Target) {
			matching = append(matching, job)
		}
	}
	if len(matching) != 2 {
		t.Fatal(matching)
	}
	if err := os.MkdirAll(destination, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, matching[0].Name), []byte("keep conflicting content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := app.Retry(context.Background()); err == nil {
		t.Fatal("collision not reported")
	}
	state, err := app.Status(context.Background())
	if err != nil || state.Pending != 2 || state.OtherTargetPending != 1 {
		t.Fatal("failed/foreign queue state incorrect", state, err)
	}
	if data, err := os.ReadFile(filepath.Join(destination, matching[1].Name)); err != nil || string(data) != "%PDF local" {
		t.Fatal("earlier failure blocked later matching job", err)
	}
	if err := os.Remove(filepath.Join(destination, matching[0].Name)); err != nil {
		t.Fatal(err)
	}
	if err := app.Retry(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err = app.Status(context.Background())
	if err != nil || state.Pending != 1 || state.OtherTargetPending != 1 || state.Error != "" {
		t.Fatal("retry did not leave only foreign target", state, err)
	}
}

type partialScan struct{}

func (partialScan) Scan(vens.ScanConfig, func(vens.Page)) ([]vens.Page, error) {
	return []vens.Page{{JPEG: []byte("partial image")}}, errors.New("terminal paper jam")
}
func TestPartialScanNeverEntersOutbox(t *testing.T) {
	dir := t.TempDir()
	app, err := New(context.Background(), dir, "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	view := app.settings.View()
	_, err = app.settings.Save(config.Update{Revision: view.Revision, FTP: config.FTP{Host: "127.0.0.1:1", User: "scan", Directory: "/"}})
	if err != nil {
		t.Fatal(err)
	}
	app.startScan(partialScan{})
	app.workers.Wait()
	files, _ := filepath.Glob(filepath.Join(dir, "outbox", "*"))
	if len(files) != 0 {
		t.Fatal("partial scan queued", files)
	}
	state, err := app.Status(context.Background())
	if err != nil || state.Error != "terminal paper jam" || state.LastPages != 1 {
		t.Fatal(state, err)
	}
}
func TestStatusUnknownWithoutRuntimeAndNoSecret(t *testing.T) {
	app, err := New(context.Background(), t.TempDir(), "test", "status")
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	state, err := app.Status(context.Background())
	if err != nil || state.Online || state.Running || state.PaperLoaded != nil {
		t.Fatal(state, err)
	}
	jobs, _ := output.Pending(app.dir)
	if len(jobs) != 0 {
		t.Fatal(jobs)
	}
	release, ok := app.guard.TryAcquire()
	if !ok {
		t.Fatal("cannot own operation")
	}
	defer release()
	if _, err := app.Discover(context.Background(), "not-an-ip"); !errors.Is(err, ErrBusy) {
		t.Fatal("discovery entered a scan", err)
	}
}
