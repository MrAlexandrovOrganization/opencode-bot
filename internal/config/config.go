package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds all bot configuration loaded from environment variables.
type Config struct {
	BotToken            string
	RootID              int64
	OpenCodeBaseURL     string
	OpenCodeUsername    string
	OpenCodePassword    string
	DefaultModel        string
	DefaultAgent        string
	PermissionMode      string
	RequestTimeout      time.Duration
	TelegramLocalAPIURL string
	WhisperGRPCHost     string
	WhisperGRPCPort     string
}

func Load() (*Config, error) {
	token := os.Getenv("BOT_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("BOT_TOKEN is required")
	}

	rootIDStr := os.Getenv("ROOT_ID")
	if rootIDStr == "" {
		return nil, fmt.Errorf("ROOT_ID is required")
	}
	rootID, err := strconv.ParseInt(rootIDStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("ROOT_ID must be a number: %w", err)
	}

	timeout, err := time.ParseDuration(getEnv("OPENCODE_REQUEST_TIMEOUT", "30m"))
	if err != nil {
		return nil, fmt.Errorf("OPENCODE_REQUEST_TIMEOUT: %w", err)
	}

	return &Config{
		BotToken:            token,
		RootID:              rootID,
		OpenCodeBaseURL:     getEnv("OPENCODE_BASE_URL", "http://localhost:4096"),
		OpenCodeUsername:    getEnv("OPENCODE_USERNAME", "opencode"),
		OpenCodePassword:    os.Getenv("OPENCODE_PASSWORD"),
		DefaultModel:        os.Getenv("OPENCODE_MODEL"),
		DefaultAgent:        getEnv("OPENCODE_AGENT", "build"),
		PermissionMode:      getEnv("PERMISSION_MODE", "ask"),
		RequestTimeout:      timeout,
		TelegramLocalAPIURL: os.Getenv("TELEGRAM_LOCAL_API_URL"),
		WhisperGRPCHost:     getEnv("WHISPER_GRPC_HOST", ""),
		WhisperGRPCPort:     getEnv("WHISPER_GRPC_PORT", "50053"),
	}, nil
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
