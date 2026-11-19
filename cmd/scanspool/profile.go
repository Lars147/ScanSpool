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

	"scanspool/internal/config"
	"scanspool/internal/control"
	"scanspool/internal/setup"
	"scanspool/internal/vens"
)

func profileCommand(args []string) error {
	options := config.ScanOptions()
	action := "show"
	if len(args) > 0 {
		if args[0] == "-h" || args[0] == "--help" {
			fmt.Println("Usage: scanspool profile <show|set|reset> [options]\nUse profile set --help for the available scan options.")
			return nil
		}
		action, args = args[0], args[1:]
	}
	if action != "show" && action != "set" && action != "reset" {
		return fmt.Errorf("unknown profile action %q; use show, set or reset", action)
	}
	flags := flag.NewFlagSet("scanspool profile "+action, flag.ContinueOnError)
	dirFlag := flags.String("data-dir", "", "persistent scanner data directory")
	asJSON := flags.Bool("json", false, "print the resulting profile as JSON")
	values := make(map[string]*string)
	if action == "set" {
		for _, option := range options {
			values[option.Key] = flags.String(option.Key, "", option.Label+" ("+option.Help+")")
		}
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected profile arguments: %v", flags.Args())
	}
	provided := make(map[string]bool)
	flags.Visit(func(f *flag.Flag) { provided[f.Name] = true })
	if action == "set" {
		hasChanges := false
		for key := range values {
			hasChanges = hasChanges || provided[key]
		}
		if !hasChanges {
			return fmt.Errorf("specify at least one scan option; see profile set --help")
		}
	}
	dir, _, err := receiverDirectory(*dirFlag)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	handle := func(view config.ProfileView, save func(config.ProfileView) (config.ProfileView, error)) error {
		switch action {
		case "set":
			for _, option := range options {
				if provided[option.Key] {
					if err := option.Set(&view.Profile, *values[option.Key]); err != nil {
						return err
					}
				}
			}
		case "reset":
			view.Profile = vens.PCReferenceConfig()
		}
		if action != "show" {
			var err error
			view, err = save(view)
			if err != nil {
				return err
			}
		}
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode(view.Profile)
		}
		for _, option := range options {
			fmt.Printf("%s: %s\n", option.Label, option.Value(view.Profile))
		}
		return nil
	}
	client, err := control.Attach(ctx, dir)
	if err == nil {
		defer client.Close()
		view, err := client.Profile(ctx)
		if err != nil {
			return err
		}
		return handle(view, func(update config.ProfileView) (config.ProfileView, error) {
			return client.SaveProfile(ctx, update)
		})
	}
	if !errors.Is(err, control.ErrUnavailable) {
		return err
	}
	if action == "show" {
		store, err := config.Open(dir)
		if err != nil {
			return err
		}
		return handle(store.Profile(), nil)
	}
	lease, err := setup.AcquireLease(dir)
	if err != nil {
		return err
	}
	defer lease.Close()
	store, err := config.Open(dir)
	if err != nil {
		return err
	}
	return handle(store.Profile(), store.SaveProfile)
}
