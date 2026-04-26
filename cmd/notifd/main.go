package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog/log"

	"github.com/avinash-shinde/notification-service/internal/api"
	"github.com/avinash-shinde/notification-service/internal/config"
	"github.com/avinash-shinde/notification-service/internal/observability"
	"github.com/avinash-shinde/notification-service/internal/worker"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	observability.InitLogger(cfg.LogLevel)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	switch os.Args[1] {
	case "api":
		if err := api.Run(ctx, cfg); err != nil {
			log.Fatal().Err(err).Msg("api stopped with error")
		}
	case "worker":
		if err := worker.Run(ctx, cfg); err != nil {
			log.Fatal().Err(err).Msg("worker stopped with error")
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: notifd <api|worker>")
}
