package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackfillCommandClassifiesConfigurationErrors(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	const secret = "synthetic-config-secret"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("CREDENTIAL_KEY=\""+secret+"\nDATABASE_URL=postgres://private:private@localhost/private\n"), 0600); err != nil {
		t.Fatal(err)
	}
	err := command([]string{"backfill", "--account", "synthetic"})
	if err == nil || err.Error() != "configuration unavailable" || strings.Contains(err.Error(), secret) {
		t.Fatalf("configuration error was not safely classified: %v", err)
	}
}

func TestBackfillCommandClassifiesDatabaseErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("DATABASE_URL", "postgres://private:synthetic-driver-secret@%not-a-host/private")
	t.Setenv("CREDENTIAL_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("TRADEWINDS_API_URL", "https://tradewinds.kibb.io")
	t.Setenv("HTTP_ADDR", "127.0.0.1:8585")
	t.Setenv("RESERVE_PENCE", "100000")
	t.Setenv("MAX_PAYBACK_GAME_DAYS", "30")
	t.Setenv("CAPACITY_PAYBACK_GAME_DAYS", "420")
	t.Setenv("MARQUES_ENABLED", "false")
	t.Setenv("REPLAY_RETENTION_DAYS", "90")
	t.Setenv("REPLAY_MAX_GIB", "20")
	err := command([]string{"backfill", "--account", "synthetic"})
	if err == nil || err.Error() != "database unavailable" || strings.Contains(err.Error(), "synthetic-driver-secret") {
		t.Fatalf("database error was not safely classified: %v", err)
	}
}
