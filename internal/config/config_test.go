package config

import (
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestReplayConfigurationLimits(t *testing.T) {
	for _, test := range []struct {
		name  string
		days  string
		gib   string
		valid bool
	}{
		{name: "defaults", valid: true},
		{name: "zero days", days: "0"},
		{name: "minimum days", days: "1", valid: true},
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
		})
	}
}

func TestLoadDoesNotExposeDotenvErrors(t *testing.T) {
	const secret = "tw_live_synthetic_dotenv_secret"
	const databasePassword = "synthetic_database_password"
	for _, test := range []struct {
		name      string
		directory bool
	}{
		{name: "malformed file"},
		{name: "unreadable file", directory: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			var err error
			if test.directory {
				err = os.Mkdir(".env", 0700)
			} else {
				contents := "CREDENTIAL_KEY=\"" + secret + "\nDATABASE_URL=postgres://private:" + databasePassword + "@localhost/private\n"
				err = os.WriteFile(".env", []byte(contents), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = Load()
			if err == nil {
				t.Fatal("invalid dotenv file was accepted")
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), databasePassword) {
				t.Fatal("dotenv error exposed a configured secret")
			}
			if err.Error() != "load .env: file could not be read or parsed" || errors.Unwrap(err) != nil {
				t.Fatalf("dotenv failure did not stop at a constant safe classification: %v", err)
			}
		})
	}
}
