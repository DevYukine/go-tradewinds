package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/config"
	"github.com/DevYukine/go-tradewinds/internal/engine"
	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/httpapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func main() {
	if err := command(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func command(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Println(`Tradewinds
  register [--handle Yukine --company 'The Pokemon Pirates' --home le_havre]
  accounts
  run [--execute]

Credentials are encrypted in PostgreSQL.
run observes by default; --execute enables trading and fleet growth.`)
		return nil
	}
	if args[0] != "register" && args[0] != "accounts" && args[0] != "run" {
		return fmt.Errorf("unknown command %q", args[0])
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log, err := newLogger()
	if err != nil {
		return err
	}
	log = log.Named("root")
	defer log.Sync() // Terminal devices may reject Sync.

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	db, err := store.Open(ctx, cfg.DatabaseURL, cfg.MasterKey, log.Named("store"))
	cancel()
	if err != nil {
		return err
	}
	defer db.Close()

	switch args[0] {
	case "register":
		return register(cfg, db, log, args[1:])
	case "accounts":
		if len(args) != 1 {
			return errors.New("accounts takes no arguments")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		accounts, err := db.Accounts(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(accounts)
	case "run":
		return run(cfg, db, log, args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func run(cfg config.Config, db *store.Store, log *zap.Logger, args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	execute := flags.Bool("execute", false, "Enable autonomous game mutations")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("run takes no positional arguments")
	}

	app := fx.New(
		fx.Supply(cfg, db, log),
		fx.WithLogger(func(log *zap.Logger) fxevent.Logger {
			events := &fxevent.ZapLogger{Logger: log.Named("fx")}
			events.UseLogLevel(zapcore.DebugLevel)
			events.UseErrorLevel(zapcore.ErrorLevel)
			return events
		}),
		fx.Invoke(func(lifecycle fx.Lifecycle, shutdown fx.Shutdowner) {
			startRuntime(lifecycle, shutdown, cfg, db, log, *execute)
		}),
	)
	app.Run()
	return app.Err()
}

func startRuntime(lifecycle fx.Lifecycle, shutdown fx.Shutdowner, cfg config.Config, db *store.Store, log *zap.Logger, execute bool) {
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	apiLog := log.Named("api")
	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpapi.NewHandler(db, apiLog, cfg.HTTPAddr),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	lifecycle.Append(fx.Hook{
		OnStart: func(startCtx context.Context) error {
			accounts, err := db.Accounts(startCtx)
			if err != nil {
				return fmt.Errorf("load runtime accounts: %w", err)
			}
			type accountRunner struct {
				runner *engine.Runner
				log    *zap.Logger
			}
			runners := make([]accountRunner, 0, len(accounts))
			for _, account := range accounts {
				if !account.Enabled {
					continue
				}
				accountLog := log.With(
					zap.String("account", account.Label),
					zap.String("account_id", account.ID),
				)
				if account.CompanyID == "" {
					accountLog.Warn("skipping account with incomplete registration; resume register")
					continue
				}
				credentials, err := db.Credentials(startCtx, account.ID)
				if err != nil {
					accountLog.Error("skipping account with unreadable credentials", zap.Error(err))
					continue
				}
				if credentials.APIKey == "" {
					accountLog.Warn("skipping account without an API key; resume register")
					continue
				}
				client := gameapi.New(cfg.APIURL, credentials.APIKey, accountLog.Named("api"))
				engineLog := accountLog.Named("engine")
				runner := engine.New(client, db, account, engineLog, engine.Options{
					Execute:             execute,
					ReservePence:        cfg.ReservePence,
					MaxPaybackDays:      cfg.MaxPaybackDays,
					CapacityPaybackDays: cfg.CapacityPaybackDays,
					SmallFleetTarget:    cfg.SmallFleetTarget,
					MarquesEnabled:      cfg.MarquesEnabled,
				})
				runners = append(runners, accountRunner{runner: runner, log: engineLog})
			}

			listener, err := net.Listen("tcp", cfg.HTTPAddr)
			if err != nil {
				return fmt.Errorf("listen for dashboard API on %s: %w", cfg.HTTPAddr, err)
			}
			for _, runner := range runners {
				workers.Add(1)
				go func() {
					defer workers.Done()
					if err := runner.runner.Run(ctx); err != nil && ctx.Err() == nil && !errors.Is(err, context.Canceled) {
						runner.log.Error("account runner stopped", zap.Error(err))
					}
				}()
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				prunePeriodically(ctx, db, log.Named("store"))
			}()
			workers.Add(1)
			go func() {
				defer workers.Done()
				apiLog.Info("dashboard API listening", zap.String("address", cfg.HTTPAddr), zap.Bool("execute", execute))
				if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
					apiLog.Error("dashboard server stopped", zap.Error(err))
					_ = shutdown.Shutdown(fx.ExitCode(1))
				}
			}()
			return nil
		},
		OnStop: func(stopCtx context.Context) error {
			cancel()
			err := server.Shutdown(stopCtx)
			if err != nil {
				err = fmt.Errorf("shut down dashboard API: %w", err)
			}
			done := make(chan struct{})
			go func() {
				workers.Wait()
				close(done)
			}()
			select {
			case <-done:
				return err
			case <-stopCtx.Done():
				return fmt.Errorf("wait for runtime workers: %w", stopCtx.Err())
			}
		},
	})
}

func prunePeriodically(ctx context.Context, db *store.Store, log *zap.Logger) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		if err := db.Prune(ctx); err != nil && ctx.Err() == nil {
			log.Warn("retention failed", zap.Error(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
