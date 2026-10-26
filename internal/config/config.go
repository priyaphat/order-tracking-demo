package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	HTTPAddr          string
	DatabaseURL       string
	InternalAPIToken  string
	BankWebhookSecret string
	LeaseDuration     time.Duration
	PaymentTTL        time.Duration
	QueueSLA          time.Duration
	SweepInterval     time.Duration
	MaxRetries        int
}

func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:          value("HTTP_ADDR", ":8080"),
		DatabaseURL:       os.Getenv("DATABASE_URL"),
		InternalAPIToken:  value("INTERNAL_API_TOKEN", "local-worker-token"),
		BankWebhookSecret: value("BANK_WEBHOOK_SECRET", "local-bank-secret"),
		MaxRetries:        intValue("MAX_RETRIES", 3),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	var err error
	if cfg.LeaseDuration, err = duration("LEASE_DURATION", 2*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.PaymentTTL, err = duration("PAYMENT_TTL", 15*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.QueueSLA, err = duration("QUEUE_SLA", 5*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.SweepInterval, err = duration("SWEEP_INTERVAL", 10*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.MaxRetries < 1 {
		return Config{}, fmt.Errorf("MAX_RETRIES must be at least 1")
	}
	return cfg, nil
}

func value(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func duration(key string, fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", key)
	}
	return v, nil
}

func intValue(key string, fallback int) int {
	v, err := strconv.Atoi(os.Getenv(key))
	if err != nil {
		return fallback
	}
	return v
}
