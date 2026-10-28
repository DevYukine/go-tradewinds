package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/pprof"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/config"
	"github.com/DevYukine/go-tradewinds/internal/sim"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
)

type gateError struct{}

func (gateError) Error() string { return "UNTRUSTED: validation gate failed" }

func main() {
	if err := command(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		var failedGate gateError
		if errors.As(err, &failedGate) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func command(args []string) error {
	return commandTo(args, os.Stdout)
}

func commandTo(args []string, output io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		_, err := fmt.Fprintln(output, "tw-sim backfill --account <label-or-ID>\ntw-sim validate [--account <label-or-ID>] [--from RFC3339] [--to RFC3339]\ntw-sim run [--account <label-or-ID>] [--from RFC3339] [--fit-until RFC3339] [--to RFC3339] [--out result.json]\nValidate and run are database-read-only and make no game requests. Comparisons are simulated estimates, not live profits.")
		return err
	}
	operation := args[0]
	if operation != "backfill" && operation != "validate" && operation != "run" {
		return fmt.Errorf("unknown command %q", operation)
	}
	flags := flag.NewFlagSet(operation, flag.ContinueOnError)
	selector := flags.String("account", "", "Account label or ID; optional when exactly one account exists")
	var fromFlag, toFlag, fitFlag, outFlag, profileFlag *string
	if operation != "backfill" {
		fromFlag = flags.String("from", "", "Inclusive real UTC start; defaults to first retained frame (first usable observation for run)")
		toFlag = flags.String("to", "", "Exclusive real UTC end; defaults to last replay frame")
	}
	if operation == "run" {
		fitFlag = flags.String("fit-until", "", "Training/holdout boundary; defaults to midpoint")
		outFlag = flags.String("out", "", "Write deterministic comparison JSON")
		profileFlag = flags.String("cpuprofile", "", "Write an offline calibration and simulation CPU profile")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || operation == "backfill" && *selector == "" {
		return errors.New("usage: tw-sim backfill --account <label-or-ID>, validate, or run; see --help")
	}
	from, to := time.Time{}, time.Now().UTC()
	if fromFlag != nil && *fromFlag != "" {
		var err error
		from, err = time.Parse(time.RFC3339Nano, *fromFlag)
		if err != nil {
			return errors.New("--from must be RFC3339")
		}
	}
	if toFlag != nil && *toFlag != "" {
		var err error
		to, err = time.Parse(time.RFC3339Nano, *toFlag)
		if err != nil {
			return errors.New("--to must be RFC3339")
		}
	}
	if !to.After(from) {
		return errors.New("--to must follow --from")
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
	var db *store.Store
	if operation == "backfill" {
		db, err = store.Open(openCtx, cfg.DatabaseURL, cfg.MasterKey, log.Named("store"))
	} else {
		db, err = store.OpenReadOnly(openCtx, cfg.DatabaseURL, cfg.MasterKey, log.Named("store"))
	}
	cancel()
	if err != nil {
		return errors.New("database unavailable")
	}
	defer db.Close()
	account, err := selectAccount(ctx, db, *selector)
	if err != nil {
		return err
	}
	if operation == "backfill" {
		report, err := sim.Backfill(ctx, db, account.ID)
		if err != nil {
			return errors.New("backfill failed")
		}
		_, err = report.WriteTo(output)
		return err
	}
	return db.WithReplaySnapshot(ctx, func(db *store.Store) error {
		// Resolve omitted bounds before loading bounded action/settlement
		// evidence: the Unix epoch would turn a default run into lifetime history.
		if *fromFlag == "" || *toFlag == "" {
			first, last, err := db.ReplayFrameBounds(ctx, account.ID, to)
			if err != nil {
				return replayEvidenceError(output, err, "replay frame bounds unavailable")
			}
			if first.IsZero() {
				return errors.New("no replay frames in the requested window")
			}
			if *fromFlag == "" {
				from = first.UTC()
			}
			if *toFlag == "" {
				to = last.UTC().Add(time.Nanosecond)
			}
		}
		if !to.After(from) {
			return errors.New("replay window is empty")
		}
		tape, err := sim.LoadTape(ctx, db, account.ID, from, to)
		if err != nil {
			return replayEvidenceError(output, err, "replay evidence unavailable")
		}
		first, _ := tape.FrameBounds()
		if first.IsZero() {
			return errors.New("no replay frames in the requested window")
		}
		if *fromFlag == "" {
			from = first.UTC()
			if operation == "run" {
				from, err = firstUsableTime(tape)
				if err != nil {
					if errors.Is(err, store.ErrReplayEvidenceBudget) {
						return replayEvidenceError(output, err, "")
					}
					return err
				}
			}
		}
		if !to.After(from) {
			return errors.New("replay window is empty")
		}
		validation, err := sim.Validate(tape, from, to)
		if err != nil {
			return replayEvidenceError(output, err, "validation failed to read model evidence")
		}
		if _, err := validation.WriteTo(output); err != nil {
			return err
		}
		if operation == "validate" {
			if !validation.Trusted {
				return gateError{}
			}
			return nil
		}
		fitUntil := from.Add(to.Sub(from) / 2)
		if *fitFlag != "" {
			fitUntil, err = time.Parse(time.RFC3339Nano, *fitFlag)
			if err != nil {
				return errors.New("--fit-until must be RFC3339")
			}
		}
		if !fitUntil.After(from) || !fitUntil.Before(to) {
			return errors.New("--fit-until must be strictly between --from and --to")
		}
		started := time.Now()
		if *profileFlag != "" {
			profile, err := os.Create(*profileFlag)
			if err != nil {
				return errors.New("CPU profile file unavailable")
			}
			if err := pprof.StartCPUProfile(profile); err != nil {
				_ = profile.Close()
				return errors.New("CPU profiler unavailable")
			}
			defer func() {
				pprof.StopCPUProfile()
				_ = profile.Close()
			}()
		}
		calibration, err := sim.Calibrate(tape, from, fitUntil)
		if err != nil {
			return replayEvidenceError(output, err, "calibration failed")
		}
		report := comparisonReport{From: from, FitUntil: fitUntil, To: to, Validation: validation, Calibration: calibration, Rows: make([]comparisonRow, 0, 15)}
		if calibration.Uncalibrated {
			fmt.Fprintf(output, "Calibration: uncalibrated prior; regeneration n=%d, scenarios=0/0.5/1, speed empty=1.0 laden=0.8\n", calibration.Samples)
		} else {
			fmt.Fprintf(output, "Calibration: regeneration n=%d p25=%.4f median=%.4f p75=%.4f\n", calibration.Samples, calibration.Conservative, calibration.Base, calibration.Optimistic)
		}
		for _, scenario := range []struct {
			name  string
			regen float64
		}{{"conservative", calibration.Conservative}, {"base", calibration.Base}, {"optimistic", calibration.Optimistic}} {
			rows, err := sim.Run(ctx, tape, sim.RunOptions{From: fitUntil, FleetAt: from, To: to, Regen: scenario.regen, ReservePence: cfg.ReservePence, CompanyID: account.CompanyID, SpeedFactors: calibration.Speeds})
			if err != nil {
				return replayEvidenceError(output, err, "simulation failed")
			}
			for _, row := range rows {
				label := "simulated estimate"
				if !validation.Trusted || !row.HazardAvailable {
					label = "UNTRUSTED"
				}
				if row.LowCoverage {
					label += "; low coverage"
				}
				report.Rows = append(report.Rows, comparisonRow{Scenario: scenario.name, Status: label, VariantResult: row})
			}
		}
		if err := writeComparison(output, report); err != nil {
			return err
		}
		if *outFlag != "" {
			encoded, err := json.MarshalIndent(report, "", "  ")
			if err != nil {
				return errors.New("comparison JSON encoding failed")
			}
			encoded = append(encoded, '\n')
			if err := os.WriteFile(*outFlag, encoded, 0600); err != nil {
				return errors.New("comparison JSON write failed")
			}
		}
		fmt.Fprintf(output, "Wall time: %s\n", time.Since(started).Round(time.Millisecond))
		if !validation.Trusted {
			return gateError{}
		}
		return nil
	})
}

func selectAccount(ctx context.Context, db *store.Store, selector string) (store.Account, error) {
	accounts, err := db.Accounts(ctx)
	if err != nil {
		return store.Account{}, errors.New("account lookup unavailable")
	}
	if selector == "" {
		if len(accounts) == 1 {
			return accounts[0], nil
		}
		return store.Account{}, errors.New("--account is required when there is not exactly one account")
	}
	for _, account := range accounts {
		if account.ID == selector {
			return account, nil
		}
	}
	var selected store.Account
	for _, account := range accounts {
		if account.Label == selector {
			if selected.ID != "" {
				return store.Account{}, errors.New("account label is ambiguous; use its ID")
			}
			selected = account
		}
	}
	if selected.ID == "" {
		return store.Account{}, errors.New("account not found")
	}
	return selected, nil
}

func firstUsableTime(tape *sim.Tape) (time.Time, error) {
	reader := tape.Reader()
	for {
		at, err := reader.NextTime()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return time.Time{}, err
		}
		view, err := reader.Advance(at)
		if err != nil {
			return time.Time{}, err
		}
		if len(view.State.Ships) != 0 && len(view.State.Vaults) != 0 && len(view.State.Catalog.Ports) != 0 && view.State.Clock.Calendar.TimeScale > 0 {
			return at.UTC(), nil
		}
	}
	return time.Time{}, errors.New("tape has no completed fleet, vault, catalog and clock observation")
}

func replayEvidenceError(output io.Writer, err error, fallback string) error {
	if !errors.Is(err, store.ErrReplayEvidenceBudget) {
		return errors.New(fallback)
	}
	if _, writeErr := fmt.Fprintf(output, "Validation gate: UNTRUSTED\nEvidence budget: %v\nNo comparison produced from incomplete evidence.\n", err); writeErr != nil {
		return writeErr
	}
	return gateError{}
}

type comparisonRow struct {
	Scenario string `json:"scenario"`
	Status   string `json:"status"`
	sim.VariantResult
}

type comparisonReport struct {
	From        time.Time            `json:"from"`
	FitUntil    time.Time            `json:"fit_until"`
	To          time.Time            `json:"to"`
	Validation  sim.ValidationReport `json:"validation"`
	Calibration sim.Calibration      `json:"calibration"`
	Rows        []comparisonRow      `json:"rows"`
}

func writeComparison(writer io.Writer, report comparisonReport) error {
	if _, err := fmt.Fprintln(writer, "Simulated estimates; hull values (identical across variants) and stores excluded from net worth. Plans require recorded Lloyd's hazard pricing; no recorded Lloyd's quotes means UNTRUSTED, never zero hazard. Omitted/optimistic: random sinkings and cargo loss, berth queues, competitor/NPC price response, partial-fill probabilities, freight, hull purchases/growth, refits, new loans, underwriting, marques, warehouses, factor hires, convoys, structural/expansion awards."); err != nil {
		return err
	}
	fmt.Fprintf(writer, "Window: fit [%s, %s), holdout [%s, %s)\n", report.From.Format(time.RFC3339Nano), report.FitUntil.Format(time.RFC3339Nano), report.FitUntil.Format(time.RFC3339Nano), report.To.Format(time.RFC3339Nano))
	fmt.Fprintln(writer, "Scenario     Variant   Final NW(p) Delta NW(p) Realized(p) Idle% Probes Spend(p) Conv Settle Observed(t) Carried(t) Dark Status")
	for _, row := range report.Rows {
		if _, err := fmt.Fprintf(writer, "%-12s %-8s %11d %11d %11d %5.1f %6d %8d %4d %6d %11d %10d %4d %s\n", row.Scenario, row.Variant, row.FinalNetWorthPence, row.DeltaVsBaselinePence, row.RealizedNetPence, row.IdlePercent, row.Probes, row.ProbeExpensePence, row.Conversions, row.SettlementCount, row.ObservedTons, row.CarriedTons, row.DarkDecisions, row.Status); err != nil {
			return err
		}
		if !row.HazardAvailable {
			if _, err := fmt.Fprintf(writer, "  %s/%s: hazard unavailable; %d evaluated plans lack recorded Lloyd's quotes.\n", row.Scenario, row.Variant, row.HazardUnavailablePlans); err != nil {
				return err
			}
		}
	}
	for _, variant := range []string{"i4", "probe0", "probe50", "probe400"} {
		better := report.Validation.Trusted
		seen := 0
		for _, row := range report.Rows {
			if row.Variant != variant {
				continue
			}
			seen++
			baselineIdle := 0.0
			baselineHazardAvailable := false
			for _, base := range report.Rows {
				if base.Scenario == row.Scenario && base.Variant == "baseline" {
					baselineIdle = base.IdlePercent
					baselineHazardAvailable = base.HazardAvailable
					break
				}
			}
			better = better && row.HazardAvailable && baselineHazardAvailable && row.DeltaVsBaselinePence > 0 && row.IdlePercent <= baselineIdle
		}
		verdict := "inconclusive"
		if better && seen == 3 {
			verdict = "better in all three simulated scenarios"
		}
		fmt.Fprintf(writer, "%s: %s\n", variant, verdict)
	}
	return nil
}
