// Command server runs the GlucoGuard API.
//
//	APP_ENV=development  (default) in-memory store if DATABASE_URL is empty, dev auth
//	APP_ENV=production   requires DATABASE_URL, real auth, and a clinically approved rule set
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/purplestack/glucoguard/internal/api"
	"github.com/purplestack/glucoguard/internal/app"
	"github.com/purplestack/glucoguard/internal/clinical"
	"github.com/purplestack/glucoguard/internal/store"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	env := getenv("APP_ENV", "development")
	addr := ":" + getenv("PORT", "8080")
	dsn := os.Getenv("DATABASE_URL")

	if env == "production" {
		// Safety gate: a medical decision engine must not serve patients on
		// unreviewed rules or with development authentication.
		switch {
		case !clinical.RuleSetClinicallyApproved || !clinical.AllMessagesApproved():
			return errors.New("refusing to start: clinical rule set " + clinical.RuleSetVersion + " is not clinician-approved")
		case dsn == "":
			return errors.New("refusing to start: DATABASE_URL is required in production")
		default:
			return errors.New("refusing to start: production authentication is not configured (DevHeaderAuth is dev-only)")
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var st store.Store
	if dsn != "" {
		pg, err := store.NewPostgres(ctx, dsn)
		if err != nil {
			return fmt.Errorf("postgres: %w", err)
		}
		st = pg
		log.Info("using postgres store")
	} else {
		st = store.NewMemory()
		log.Warn("using IN-MEMORY store: data is lost on restart (development only)")
	}
	defer st.Close()

	svc := app.New(st, log)
	worker := &app.ReminderWorker{Svc: svc, Notifier: app.LogNotifier{Log: log}}
	go worker.Run(ctx)

	srv := &http.Server{
		Addr:              addr,
		Handler:           api.NewServer(svc, api.DevHeaderAuth{}, log).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Warn("clinical rule set is DRAFT — not for patient use", "rule_set_version", clinical.RuleSetVersion)
	log.Info("listening", "addr", addr, "env", env)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
