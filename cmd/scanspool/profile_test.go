package main

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"scanspool/internal/config"
	"scanspool/internal/control"
	"scanspool/internal/service"
	"scanspool/internal/vens"
)

func TestProfileCommandOfflineAndLive(t *testing.T) {
	dir := t.TempDir()
	set := []string{"profile", "set", "--data-dir", dir}
	if err := command(append(set, "--color", "bw", "--dpi", "200", "--sides", "simplex",
		"--paper", "a5", "--width", "1000", "--height", "2000", "--compression", "4",
		"--bw-density", "-3", "--bleed-through", "on", "--multi-feed", "off", "--blank-pages", "on")); err != nil {
		t.Fatal(err)
	}
	store, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := vens.ScanConfig{ColorMode: vens.ColorBW, Quality: vens.QualityFine, PaperSize: vens.PaperA5,
		PaperWidth: 1000, PaperHeight: 2000, CompressionArg: 10, BWDensity: -3, BleedThrough: true, BlankPageRemoval: true}
	original := store.Profile()
	if original.Profile != want {
		t.Fatalf("saved %+v, want %+v", original.Profile, want)
	}
	if err := command(append(set, "--color", "auto")); err == nil {
		t.Fatal("accepted incompatible profile")
	}
	ctx := context.Background()
	app, err := service.New(ctx, dir, "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if _, err := app.Save(ctx, config.Update{Revision: store.View().Revision, Kind: config.KindLocal, LocalDirectory: filepath.Join(dir, "finished")}); err != nil {
		t.Fatal(err)
	}
	server, err := control.Listen(ctx, dir, app)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	defer func() {
		server.Close()
		if err := <-done; err != http.ErrServerClosed {
			t.Error(err)
		}
	}()
	if err := command(append(set, "--multi-feed", "on")); err != nil {
		t.Fatal(err)
	}
	got, err := app.Profile(ctx)
	want.MultiFeed = true
	if err != nil || got.Profile != want {
		t.Fatal("live receiver did not load CLI changes", got, err)
	}
	if _, err := app.SaveProfile(ctx, original); err != config.ErrConflict {
		t.Fatal("stale profile could overwrite CLI changes", err)
	}
	if err := command([]string{"profile", "reset", "--data-dir", dir}); err != nil {
		t.Fatal(err)
	}
	saved, err := config.Open(dir)
	if err != nil || saved.Profile().Profile != vens.PCReferenceConfig() || saved.Get().OutputKind() != config.KindLocal {
		t.Fatal("reset failed or changed the output destination", err)
	}
}
