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
		target  string
		marques string
		valid   bool
	}{
		{name: "defaults", valid: true},
		{name: "upper payback boundary", payback: "720", target: "1", valid: true},
		{name: "marques enabled", marques: "true", valid: true},
		{name: "zero payback", payback: "0"},
		{name: "negative payback", payback: "-1"},
		{name: "excessive payback", payback: "720.1"},
		{name: "nonfinite payback", payback: "NaN"},
		{name: "infinite payback", payback: "+Inf"},
		{name: "zero target", target: "0"},
		{name: "negative target", target: "-1"},
		{name: "fractional target", target: "2.5"},
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
			t.Setenv("SMALL_FLEET_TARGET", test.target)
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
			if test.name == "defaults" && (cfg.CapacityPaybackDays != 180 || cfg.SmallFleetTarget != 5 || cfg.MarquesEnabled) {
				t.Fatalf("defaults = %v days, %d ships, marques %v", cfg.CapacityPaybackDays, cfg.SmallFleetTarget, cfg.MarquesEnabled)
			}
			if test.name == "marques enabled" && !cfg.MarquesEnabled {
				t.Fatal("MARQUES_ENABLED=true did not enable marques")
			}
			if test.name == "upper payback boundary" && (cfg.CapacityPaybackDays != 720 || cfg.SmallFleetTarget != 1) {
				t.Fatalf("capacity boundary values = %v days, %d ships", cfg.CapacityPaybackDays, cfg.SmallFleetTarget)
			}
		})
	}
}
