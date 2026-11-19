package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"scanspool/internal/config"
	"scanspool/internal/service"
)

type scanAPI struct {
	service.API
	calls atomic.Int32
	busy  atomic.Bool
}

func (a *scanAPI) StartScan(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.calls.Add(1)
	if a.busy.Load() {
		return service.ErrBusy
	}
	return nil
}

func TestScanEndpointAuthenticationAcceptanceAndBusyResponse(t *testing.T) {
	api := &scanAPI{}
	server := httptest.NewServer(handler(api, "test-token"))
	defer server.Close()
	response, err := http.Post(server.URL+"/v1/scan", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || api.calls.Load() != 0 {
		t.Fatal("unauthenticated scan started")
	}
	client := &Client{http: server.Client(), info: endpoint{Address: strings.TrimPrefix(server.URL, "http://"), Token: "test-token"}}
	api.busy.Store(true)
	if err := client.StartScan(context.Background()); !errors.Is(err, service.ErrBusy) {
		t.Fatal("lost typed busy response", err)
	}
	api.busy.Store(false)
	if err := client.StartScan(context.Background()); err != nil {
		t.Fatal("scan acceptance failed", err)
	}
	if api.calls.Load() != 2 {
		t.Fatal("wrong request count", api.calls.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.StartScan(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled request accepted", err)
	}
}

func TestLocalAdministrationAndSlowBodyIsolation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	app, err := service.New(ctx, dir, "test", "run")
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	server, err := Listen(ctx, dir, app)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	defer func() { server.Close(); <-done }()
	client, err := Attach(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	view, err := client.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	view, err = client.Save(ctx, config.Update{Revision: view.Revision, PasswordAction: "set", FTP: config.FTP{Host: "ftp.test", User: "scan", Password: "synthetic-secret", Directory: "/consume"}})
	if err != nil || !view.HasPassword || view.FTP.Password != "" {
		t.Fatal("secret leaked or config failed", err)
	}
	data, _ := json.Marshal(view)
	if strings.Contains(string(data), "synthetic-secret") {
		t.Fatal("secret in response")
	}
	if _, err := client.Save(ctx, config.Update{Revision: "stale"}); !errors.Is(err, config.ErrConflict) {
		t.Fatal("lost typed conflict", err)
	}
	previous := view
	localDirectory := filepath.Join(dir, "finished")
	view, err = client.Save(ctx, config.Update{Revision: view.Revision, Kind: config.KindLocal, LocalDirectory: localDirectory})
	if err != nil || view.Kind != config.KindLocal || view.LocalDirectory != localDirectory || !view.HasPassword || view.FTP.Password != "" || view.FTP.Host != previous.FTP.Host {
		t.Fatal("local control roundtrip lost settings or leaked secret", view, err)
	}
	if _, err := client.Save(ctx, config.Update{Revision: previous.Revision, Kind: config.KindLocal, LocalDirectory: filepath.Join(dir, "other")}); !errors.Is(err, config.ErrConflict) {
		t.Fatal("local control update lost typed conflict", err)
	}
	status, err := client.Status(ctx)
	if err != nil || !status.OutputReady || status.FTPReady || status.OutputKind != config.KindLocal || status.OutputDirectory != localDirectory {
		t.Fatal("local status roundtrip failed", status, err)
	}
	// A request blocked while sending its body must not enter an application operation.
	slow, err := net.Dial("tcp4", server.info.Address)
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	fmt.Fprintf(slow, "PUT /v1/settings HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nContent-Length: 1000\r\n\r\n{", server.info.Token)
	probe, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	if _, err := client.Status(probe); err != nil {
		t.Fatal("incomplete body blocked status", err)
	}
	slow.SetReadDeadline(time.Now().Add(7 * time.Second))
	response, err := io.ReadAll(slow)
	if err != nil || !strings.Contains(string(response), "400 Bad Request") {
		t.Fatal("body deadline missing", err, string(response))
	}
	unauthorized, err := http.Get("http://" + server.info.Address + "/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != 401 {
		t.Fatal("missing authentication")
	}
	info := server.info
	info.Token = strings.Repeat("0", 64)
	data, _ = json.Marshal(info)
	if err := os.WriteFile(endpointPath(dir), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Attach(ctx, dir); err == nil || errors.Is(err, ErrUnavailable) {
		t.Fatal("authentication failure allowed standalone fallback", err)
	}
}
func TestEndpointValidation(t *testing.T) {
	dir := t.TempDir()
	if _, err := Attach(context.Background(), dir); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if err := os.WriteFile(endpointPath(dir), []byte(`{"address":"192.0.2.1:1234","token":"`+strings.Repeat("a", 64)+`","protocol":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Attach(context.Background(), dir); err == nil || errors.Is(err, ErrUnavailable) {
		t.Fatal("invalid endpoint treated as offline", err)
	}
}
