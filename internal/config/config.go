package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type Config struct {
	Port              string
	DatabaseURL       string
	JWTSecret         []byte
	SaltRounds        int
	ValidationTimeout time.Duration
	LogLevel          slog.Level
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func Load() (Config, error) {
	cfg := Config{
		Port:        getenv("APP_PORT", "8080"),
		DatabaseURL: getenv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/ticketbooking?sslmode=disable"),
		JWTSecret:   []byte(os.Getenv("jwt_secret")),
	}
	if err := cfg.LogLevel.UnmarshalText([]byte(getenv("LOG_LEVEL", "info"))); err != nil {
		return cfg, errors.New("LOG_LEVEL must be debug, info, warn or error")
	}
	if len(cfg.JWTSecret) == 0 {
		return cfg, errors.New("jwt_secret is required")
	}

	rounds, err := strconv.Atoi(getenv("salt_rounds", "10"))
	if err != nil || rounds < bcrypt.MinCost || rounds > bcrypt.MaxCost {
		return cfg, fmt.Errorf("salt_rounds must be an integer between %d and %d", bcrypt.MinCost, bcrypt.MaxCost)
	}
	cfg.SaltRounds = rounds

	timeout, err := time.ParseDuration(getenv("validation_timeout", "15m"))
	if err != nil || timeout <= 0 {
		return cfg, errors.New("validation_timeout must be a positive duration such as 15m")
	}
	cfg.ValidationTimeout = timeout

	return cfg, nil
}
