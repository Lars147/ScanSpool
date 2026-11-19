package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"golang.org/x/term"
	"scanspool/internal/control"
	"scanspool/internal/daemon"
	"scanspool/internal/service"
	"scanspool/internal/setup"
	"scanspool/internal/tui"
)

var version = "dev"

func main() {
	if err := command(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		os.Exit(1)
	}
}
func dataDirectory(explicit string) (string, error) {
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	if dir := os.Getenv("SCANSPOOL_DATA_DIR"); dir != "" {
		return filepath.Abs(dir)
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "scanspool"), nil
}

func receiverDirectory(explicit string) (string, bool, error) {
	if runtime.GOOS != "linux" {
		dir, err := dataDirectory(explicit)
		return dir, false, err
	}
	// Resolve the default without accessing or creating the selected directory.
	base, err := os.UserConfigDir()
	if err != nil && explicit == "" && os.Getenv("SCANSPOOL_DATA_DIR") == "" {
		return "", false, err
	}
	return daemon.New().ResolveDirectory(explicit, os.Getenv("SCANSPOOL_DATA_DIR"), filepath.Join(base, "scanspool"))
}

// Setup owns the terminal, including while its scanner workers start and stop.
// The default slog handler also uses log's writer, so redirect both to a private
// append-only log and restore stderr only after the service has shut down.
func setupLogging(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, "scanspool.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("open setup log: %w", err)
	}
	previous := log.Writer()
	log.SetOutput(file)
	return func() {
		log.SetOutput(previous)
		file.Close()
	}, nil
}

func command(args []string) error {
	if len(args) > 0 && args[0] == "service" {
		return serviceCommand(args[1:])
	}
	if len(args) > 0 && args[0] == "profile" {
		return profileCommand(args[1:])
	}
	name := "setup"
	if len(args) > 0 {
		switch args[0] {
		case "--version", "version":
			fmt.Println(version)
			return nil
		case "run", "setup", "status", "retry", "scan":
			name = args[0]
			args = args[1:]
		}
	}
	flags := flag.NewFlagSet("scanspool "+name, flag.ContinueOnError)
	dirFlag := flags.String("data-dir", "", "persistent scanner identity, configuration and outbox")
	asJSON := flags.Bool("json", false, "machine-readable status without credentials")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: scanspool [setup|run|status|retry|scan] [--data-dir PATH] [--json]\n       scanspool profile <show|set|reset> [options]\n       scanspool service <install|start|stop|restart|status|logs|uninstall> [options]\nNo command opens terminal setup. run receives scanner button presses.\nscan starts one scan on a running receiver or open setup session.")
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	if name == "setup" && (!term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd()))) {
		return fmt.Errorf("setup needs an interactive terminal; use status --json or run without a terminal")
	}
	dir, managed, err := receiverDirectory(*dirFlag)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if name != "run" {
		client, err := control.Attach(ctx, dir)
		if err == nil {
			defer client.Close()
			switch name {
			case "setup":
				return tui.Run(ctx, client, version)
			case "retry":
				return client.Retry(ctx)
			case "scan":
				if err := client.StartScan(ctx); err != nil {
					return err
				}
				fmt.Println("Scan started. Check status for completion, output and errors.")
				return nil
			default:
				return printStatus(ctx, client, *asJSON)
			}
		}
		if !errors.Is(err, control.ErrUnavailable) {
			return err
		}
		if name == "scan" {
			if managed {
				return fmt.Errorf("no running receiver; start scanspool service start first")
			}
			return fmt.Errorf("no running receiver; start scanspool run or setup with the same --data-dir first")
		}
		if name == "setup" && managed {
			return fmt.Errorf("installed service is unavailable; use scanspool service start (with administrator rights), or inspect service status/logs")
		}
	}
	if name == "setup" {
		restoreLogging, err := setupLogging(dir)
		if err != nil {
			return err
		}
		defer restoreLogging()
	}
	lease, err := setup.AcquireLease(dir)
	if err != nil {
		return err
	}
	defer lease.Close()
	app, err := service.New(ctx, dir, version, name)
	if err != nil {
		return err
	}
	defer app.Close()
	switch name {
	case "status":
		return printStatus(ctx, app, *asJSON)
	case "retry":
		return app.Retry(ctx)
	}
	if err := app.Start(); err != nil {
		slog.Warn("scanner activation failed", "error", err)
	}
	private, err := control.Listen(ctx, dir, app)
	if err != nil {
		return err
	}
	defer private.Close()
	result := make(chan error, 1)
	go func() { result <- private.Serve() }()
	if name == "setup" {
		return tui.Run(ctx, app, version)
	}
	fmt.Fprintln(os.Stderr, "ScanSpool running. Open scanspool setup with the same --data-dir for configuration and live status.")
	select {
	case <-ctx.Done():
		return nil
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
func printStatus(ctx context.Context, api service.API, asJSON bool) error {
	status, err := api.Status(ctx)
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(status)
	}
	fmt.Printf("Running: %t\nScanner: %s (%s)\nOnline: %t\nSerial: %s\nFirmware: %s\nOutput: %s\nOutput configured: %t\nOutput directory: %s\nFTP configured: %t\nQueued PDFs: %d\nQueued for other targets: %d\nLast scan: %s (%d pages)\nError: %s\n", status.Running, status.Model, status.Host, status.Online, status.Serial, status.Firmware, status.OutputKind, status.OutputReady, status.OutputDirectory, status.FTPReady, status.Pending, status.OtherTargetPending, status.LastScan, status.LastPages, status.Error)
	if !status.Running {
		fmt.Println("No running service: scanner status is unknown. Start scanspool run for live status.")
	}
	return nil
}
