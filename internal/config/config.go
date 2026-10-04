package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Port        string
	DatabaseURL string
	AdminToken  string
	JWTSecret   string
	DBMaxConns  int32
	LogLevel    slog.Level
}

func Load() (Config, error) {
	cfg := Config{
		Port:        env("PORT", "8080"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		AdminToken:  os.Getenv("ADMIN_TOKEN"),
		JWTSecret:   os.Getenv("JWT_SECRET"),
		DBMaxConns:  20,
		LogLevel:    slog.LevelInfo,
	}
	if v := os.Getenv("DB_MAX_CONNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			return Config{}, fmt.Errorf("DB_MAX_CONNS must be an integer from 1 to 100")
		}
		cfg.DBMaxConns = int32(n)
	}
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "", "info":
		cfg.LogLevel = slog.LevelInfo
	case "debug":
		cfg.LogLevel = slog.LevelDebug
	case "warn", "warning":
		cfg.LogLevel = slog.LevelWarn
	case "error":
		cfg.LogLevel = slog.LevelError
	default:
		return Config{}, fmt.Errorf("LOG_LEVEL must be debug, info, warn, or error")
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	// The public Vercel project may only have DATABASE_URL set. These are the
	// same demo values published in the README and render.yaml. Local and Docker
	// still require the variables.
	if os.Getenv("VERCEL") == "1" {
		if cfg.AdminToken == "" {
			cfg.AdminToken = "paytm-demo-admin"
		}
		if cfg.JWTSecret == "" {
			cfg.JWTSecret = "paytm-demo-jwt-secret-change-me"
		}
	}
	if len(cfg.AdminToken) < 8 {
		return Config{}, fmt.Errorf("ADMIN_TOKEN must be at least 8 characters")
	}
	if len(cfg.JWTSecret) < 16 {
		return Config{}, fmt.Errorf("JWT_SECRET must be at least 16 characters")
	}
	return cfg, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
