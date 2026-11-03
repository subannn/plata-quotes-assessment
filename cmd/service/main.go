package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"plata-quotes-assessment/api"
	"plata-quotes-assessment/implementations"
	"plata-quotes-assessment/internal/config"
	"plata-quotes-assessment/internal/database"
	"plata-quotes-assessment/internal/exchangerates"
)

// shutdownTimeout — сколько ждать завершения текущих HTTP-запросов при остановке.
const shutdownTimeout = 10 * time.Second

func main() {
	configPath := flag.String("config", "config.json", "path to JSON config")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if err := run(*configPath, log); err != nil {
		log.Error("service stopped with error", "error", err)
		os.Exit(1)
	}
}

func run(configPath string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := database.New(ctx, cfg.Database.URL)
	if err != nil {
		return err
	}
	defer db.Close()

	provider := exchangerates.NewClient(cfg.ExchangeRates.URL, cfg.ExchangeRates.AccessKey, nil)

	svc := implementations.NewService(db, db, cfg.HTTP.APIKey, log.With("component", "api"))
	handler, err := api.NewServer(svc, svc)
	if err != nil {
		return fmt.Errorf("create api server: %w", err)
	}

	srv := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	worker := implementations.NewWorker(db, provider, implementations.WorkerConfig{
		PollInterval: time.Duration(cfg.Worker.PollInterval),
		BatchSize:    cfg.Worker.BatchSize,
		MaxAttempts:  cfg.Worker.MaxAttempts,
		LockTimeout:  time.Duration(cfg.Worker.LockTimeout),
		RateTTL:      time.Duration(cfg.Worker.RateTTL),
	}, log.With("component", "worker"))

	var wg sync.WaitGroup
	wg.Go(func() {
		worker.Run(ctx)
	})

	serverErr := make(chan error, 1)
	go func() {
		log.Info("http server started", "addr", cfg.HTTP.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	var runErr error
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-serverErr:
		runErr = fmt.Errorf("http server: %w", err)
		stop()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("http server shutdown", "error", err)
	}

	// Воркер доделывает текущую пачку и выходит.
	wg.Wait()
	log.Info("stopped")

	return runErr
}
