package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

type Config struct {
	DatabaseURL         string
	MasterKey           []byte
	APIURL              string
	HTTPAddr            string
	ReservePence        int64
	MaxPaybackDays      float64
	CapacityPaybackDays float64
	MarquesEnabled      bool
	ReplayRetentionDays int
	ReplayMaxGiB        float64
}

func Load() (Config, error) {
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		// Parser errors can contain dotenv values or the remaining file contents.
		return Config{}, errors.New("load .env: file could not be read or parsed")
	}
	cfg := Config{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		APIURL:      env("TRADEWINDS_API_URL", "https://tradewinds.kibb.io"),
		HTTPAddr:    env("HTTP_ADDR", "127.0.0.1:8585"),
	}
	if cfg.DatabaseURL == "" {
		return cfg, errors.New("DATABASE_URL is required")
	}
	key, err := base64.StdEncoding.DecodeString(os.Getenv("CREDENTIAL_KEY"))
	if err != nil || len(key) != 32 {
		return cfg, errors.New("CREDENTIAL_KEY must be a base64-encoded 32-byte key; keep it backed up with the database")
	}
	cfg.MasterKey = key

	apiURL, err := url.Parse(cfg.APIURL)
	if err != nil || apiURL.Host == "" || apiURL.User != nil || (apiURL.Scheme != "https" && apiURL.Scheme != "http") {
		return cfg, errors.New("TRADEWINDS_API_URL must be an HTTP(S) URL without credentials")
	}
	host, _, err := net.SplitHostPort(cfg.HTTPAddr)
	if err != nil {
		return cfg, fmt.Errorf("HTTP_ADDR: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return cfg, errors.New("HTTP_ADDR must bind a loopback IP; use an authenticated reverse proxy for remote access")
	}

	cfg.ReservePence, err = strconv.ParseInt(env("RESERVE_PENCE", "100000"), 10, 64)
	if err != nil || cfg.ReservePence < 0 {
		return cfg, errors.New("RESERVE_PENCE must be a nonnegative integer")
	}
	cfg.MaxPaybackDays, err = strconv.ParseFloat(env("MAX_PAYBACK_GAME_DAYS", "30"), 64)
	if err != nil || math.IsNaN(cfg.MaxPaybackDays) || cfg.MaxPaybackDays <= 0 || cfg.MaxPaybackDays > 365 {
		return cfg, errors.New("MAX_PAYBACK_GAME_DAYS must be within (0, 365]")
	}
	cfg.CapacityPaybackDays, err = strconv.ParseFloat(env("CAPACITY_PAYBACK_GAME_DAYS", "420"), 64)
	if err != nil || math.IsNaN(cfg.CapacityPaybackDays) || cfg.CapacityPaybackDays <= 0 || cfg.CapacityPaybackDays > 720 {
		return cfg, errors.New("CAPACITY_PAYBACK_GAME_DAYS must be within (0, 720]")
	}
	cfg.MarquesEnabled, err = strconv.ParseBool(env("MARQUES_ENABLED", "false"))
	if err != nil {
		return cfg, errors.New("MARQUES_ENABLED must be true or false")
	}
	cfg.ReplayRetentionDays, err = strconv.Atoi(env("REPLAY_RETENTION_DAYS", "90"))
	if err != nil || cfg.ReplayRetentionDays < 0 || cfg.ReplayRetentionDays > 106751 {
		return cfg, errors.New("REPLAY_RETENTION_DAYS must be an integer within [0, 106751]; 0 disables recording and pruning")
	}
	cfg.ReplayMaxGiB, err = strconv.ParseFloat(env("REPLAY_MAX_GIB", "20"), 64)
	if err != nil || math.IsNaN(cfg.ReplayMaxGiB) || math.IsInf(cfg.ReplayMaxGiB, 0) || cfg.ReplayMaxGiB <= 0 || cfg.ReplayMaxGiB >= float64(math.MaxInt64)/(1<<30) {
		return cfg, errors.New("REPLAY_MAX_GIB must be a positive finite number below 8589934592")
	}
	return cfg, nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
