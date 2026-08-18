package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"opencode-bot/internal/bot"
	"opencode-bot/internal/config"
	"opencode-bot/internal/opencode"
	"opencode-bot/internal/whisper"

	"github.com/mymmrac/telego"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config", "error", err)
		os.Exit(1)
	}

	httpClient := &http.Client{
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			DialContext:         (&net.Dialer{Timeout: 60 * time.Second}).DialContext,
			TLSHandshakeTimeout: 60 * time.Second,
		},
		Timeout: 120 * time.Second,
	}

	options := []telego.BotOption{telego.WithHTTPClient(httpClient)}
	if cfg.TelegramLocalAPIURL != "" {
		options = append(options, telego.WithAPIServer(cfg.TelegramLocalAPIURL))
		slog.Info("using local Telegram Bot API", "url", cfg.TelegramLocalAPIURL)
	}

	api, err := telego.NewBot(cfg.BotToken, options...)
	if err != nil {
		slog.Error("bot init", "error", err)
		os.Exit(1)
	}

	oc := opencode.New(cfg.OpenCodeBaseURL, cfg.OpenCodeUsername, cfg.OpenCodePassword)
	if err := oc.Health(context.Background()); err != nil {
		slog.Warn("opencode server unreachable", "url", cfg.OpenCodeBaseURL, "error", err)
	}

	var whisperClient *whisper.Client
	if cfg.WhisperGRPCHost != "" {
		whisperClient, err = whisper.NewClient(cfg.WhisperGRPCHost, cfg.WhisperGRPCPort)
		if err != nil {
			slog.Error("whisper client", "error", err)
			os.Exit(1)
		}
		defer whisperClient.Close()
		slog.Info("whisper configured", "host", cfg.WhisperGRPCHost, "port", cfg.WhisperGRPCPort)
	}

	b := bot.New(api, oc, whisperClient, cfg)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	slog.Info("starting opencode bot")
	b.Run(ctx)
	slog.Info("stopped")
}
