package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestCapacityGrowthConfiguration(t *testing.T) {
	for _, test := range []struct {
		name    string
		payback string
		marques string
		valid   bool
	}{
		{name: "defaults", valid: true},
		{name: "upper payback boundary", payback: "720", valid: true},
		{name: "marques enabled", marques: "true", valid: true},
		{name: "zero payback", payback: "0"},
		{name: "negative payback", payback: "-1"},
		{name: "excessive payback", payback: "720.1"},
		{name: "nonfinite payback", payback: "NaN"},
		{name: "infinite payback", payback: "+Inf"},
		{name: "unparseable marque switch", marques: "sometimes"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://localhost/capacity_config_test")
			t.Setenv("CREDENTIAL_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
			t.Setenv("HTTP_ADDR", "127.0.0.1:8585")
			t.Setenv("TRADEWINDS_API_URL", "https://tradewinds.kibb.io")
			t.Setenv("RESERVE_PENCE", "100000")
			t.Setenv("MAX_PAYBACK_GAME_DAYS", "30")
			t.Setenv("CAPACITY_PAYBACK_GAME_DAYS", test.payback)
			t.Setenv("MARQUES_ENABLED", test.marques)
			cfg, err := Load()
			if !test.valid {
				if err == nil {
					t.Fatal("invalid capacity settings were accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.name == "defaults" && (cfg.CapacityPaybackDays != 420 || cfg.MarquesEnabled) {
				t.Fatalf("defaults = %v days, marques %v", cfg.CapacityPaybackDays, cfg.MarquesEnabled)
			}
			if test.name == "marques enabled" && !cfg.MarquesEnabled {
				t.Fatal("MARQUES_ENABLED=true did not enable marques")
			}
			if test.name == "upper payback boundary" && cfg.CapacityPaybackDays != 720 {
				t.Fatalf("capacity boundary value = %v days", cfg.CapacityPaybackDays)
			}
		})
	}
}

func TestReplayConfigurationLimits(t *testing.T) {
	for _, test := range []struct {
		name  string
		days  string
		gib   string
		valid bool
	}{
		{name: "defaults", valid: true},
		{name: "disabled", days: "0", valid: true},
		{name: "fractional GiB", days: "30", gib: "0.5", valid: true},
		{name: "negative days", days: "-1"},
		{name: "fractional days", days: "0.5"},
		{name: "duration overflow", days: "106752"},
		{name: "zero cap", gib: "0"},
		{name: "negative cap", gib: "-1"},
		{name: "NaN cap", gib: "NaN"},
		{name: "infinite cap", gib: "+Inf"},
		{name: "cap overflow", gib: "8589934592"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://localhost/replay_config_test")
			t.Setenv("CREDENTIAL_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
			t.Setenv("REPLAY_RETENTION_DAYS", test.days)
			t.Setenv("REPLAY_MAX_GIB", test.gib)
			cfg, err := Load()
			if !test.valid {
				if err == nil {
					t.Fatal("invalid replay settings accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.name == "defaults" && (cfg.ReplayRetentionDays != 90 || cfg.ReplayMaxGiB != 20) {
				t.Fatalf("replay defaults: %+v", cfg)
			}
			if test.name == "disabled" && cfg.ReplayRetentionDays != 0 {
				t.Fatal("zero retention did not disable replay")
			}
		})
	}
}
