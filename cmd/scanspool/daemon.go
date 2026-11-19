package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"scanspool/internal/daemon"
)

func serviceCommand(args []string) error {
	usage := func() {
		fmt.Fprintln(os.Stderr, "Usage: scanspool service <install|start|stop|restart|status|logs|uninstall>\n  install [--user USER] [--data-dir ABSOLUTE_PATH]\n  status [--json]\n  logs [--follow]\nInstallation enables autostart; use service start separately. Updates require service restart.")
	}
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		usage()
		return nil
	}
	action := args[0]
	flags := flag.NewFlagSet("scanspool service "+action, flag.ContinueOnError)
	flags.Usage = usage
	var username, directory string
	var follow, asJSON bool
	switch action {
	case "install":
		flags.StringVar(&username, "user", "", "existing non-root receiver account (default: SUDO_USER)")
		flags.StringVar(&directory, "data-dir", "", "existing absolute scanner data directory; retained on updates")
	case "status":
		flags.BoolVar(&asJSON, "json", false, "machine-readable service status")
	case "logs":
		flags.BoolVar(&follow, "follow", false, "follow journal output")
	case "start", "stop", "restart", "uninstall":
	default:
		return fmt.Errorf("unknown service action %q", action)
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected service arguments: %v", flags.Args())
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	m := daemon.New()
	switch action {
	case "install":
		if err := m.Install(ctx, username, directory); err != nil {
			return err
		}
		fmt.Println("Service installed; autostart enabled. Close standalone setup/run, then use service start. For updates, use service restart when no scan is in progress.")
	case "status":
		s, err := m.Status(ctx)
		if err != nil {
			return err
		}
		if asJSON {
			return json.NewEncoder(os.Stdout).Encode(s)
		}
		fmt.Printf("Installed: %t\nAutostart: %t\nState: %s\nUser: %s\nData directory: %s\n", s.Installed, s.Enabled, s.Active, s.User, s.DataDir)
	case "logs":
		return m.Logs(ctx, follow)
	case "uninstall":
		if err := m.Uninstall(ctx); err != nil {
			return err
		}
		fmt.Println("Service uninstalled. Scanner registration, configuration, outbox and output files retained.")
	default:
		if err := m.Control(ctx, action); err != nil {
			return err
		}
		if action == "stop" {
			fmt.Println("Service stopped; autostart remains enabled.")
		} else {
			fmt.Println("Service receiver ready. Use setup or status for scanner availability.")
		}
	}
	return nil
}
