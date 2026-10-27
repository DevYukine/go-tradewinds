package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/config"
	"github.com/DevYukine/go-tradewinds/internal/sim"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
)

func main() {
	if err := command(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func command(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Println("tw-sim backfill --account <label-or-ID>\nImports surviving diagnostics into the encrypted replay tape. Reruns are idempotent.")
		return nil
	}
	if args[0] != "backfill" {
		return fmt.Errorf("unknown command %q; only backfill is available", args[0])
	}
	flags := flag.NewFlagSet("backfill", flag.ContinueOnError)
	selector := flags.String("account", "", "Account label or ID")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *selector == "" || flags.NArg() != 0 {
		return errors.New("usage: tw-sim backfill --account <label-or-ID>")
	}
	cfg, err := config.Load()
	if err != nil {
		return errors.New("configuration unavailable")
	}
	log, err := zap.NewProduction()
	if err != nil {
		return errors.New("logging unavailable")
	}
	defer log.Sync()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	openCtx, cancel := context.WithTimeout(ctx, time.Minute)
	db, err := store.Open(openCtx, cfg.DatabaseURL, cfg.MasterKey, log.Named("store"))
	cancel()
	if err != nil {
		return errors.New("database unavailable")
	}
	defer db.Close()
	accounts, err := db.Accounts(ctx)
	if err != nil {
		return errors.New("account lookup unavailable")
	}
	var accountID string
	for _, account := range accounts {
		if account.ID == *selector {
			accountID = account.ID
			break
		}
	}
	if accountID == "" {
		for _, account := range accounts {
			if account.Label == *selector {
				if accountID != "" {
					return errors.New("account label is ambiguous; use its ID")
				}
				accountID = account.ID
			}
		}
	}
	if accountID == "" {
		return errors.New("account not found")
	}
	report, err := sim.Backfill(ctx, db, accountID)
	if err != nil {
		return errors.New("backfill failed")
	}
	_, err = report.WriteTo(os.Stdout)
	return err
}
