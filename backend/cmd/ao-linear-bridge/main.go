// ao-linear-bridge is an opt-in, single-workspace coordinator/worker pilot.
// It never changes the AO daemon's listeners or invokes an agent directly.
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
	"path/filepath"
	"syscall"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/linearbridge"
)

func main() {
	if err := run(); err != nil {
		slog.Error("bridge stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: ao-linear-bridge coordinator|worker|reconcile [flags]")
	}
	mode := os.Args[1]
	if mode != "coordinator" && mode != "worker" && mode != "reconcile" {
		return errors.New("expected coordinator, worker or reconcile")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	data := os.Getenv("AO_DATA_DIR")
	if data == "" {
		data = filepath.Join(home, ".ao")
	}
	flags := flag.NewFlagSet(mode, flag.ContinueOnError)
	dbName := mode
	if mode == "reconcile" {
		dbName = "worker"
	}
	dbPath := flags.String("db", filepath.Join(data, "linear-bridge", dbName+".db"), "Durable SQLite file (under AO_DATA_DIR)")
	listen := flags.String("listen", "127.0.0.1:8787", "Coordinator listen address; use an HTTPS reverse proxy")
	coordinator := flags.String("coordinator", os.Getenv("AO_BRIDGE_URL"), "Coordinator origin URL")
	project := flags.String("project", os.Getenv("AO_BRIDGE_PROJECT_ID"), "Registered local AO project ID")
	harness := flags.String("harness", "", "Agent harness; empty uses project default")
	runPath := os.Getenv("AO_RUN_FILE")
	if runPath == "" {
		runPath = filepath.Join(data, "running.json")
	}
	runFile := flags.String("run-file", runPath, "AO daemon discovery file")
	task := flags.String("task", "", "Linear session ID to reconcile")
	session := flags.String("session", "", "Existing AO session ID to attach")
	if err := flags.Parse(os.Args[2:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	// A kernel advisory lock prevents two processes from using the same journal.
	// The platform implementation releases the lock automatically on process exit.
	release, err := lock(*dbPath + ".lock")
	if err != nil {
		return err
	}
	defer release()
	store, err := linearbridge.Open(*dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client := &http.Client{Timeout: 120 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	token := os.Getenv("AO_BRIDGE_WORKER_TOKEN")
	if mode != "coordinator" {
		worker := &linearbridge.Worker{Store: store, Coordinator: *coordinator, Token: token, ProjectID: *project, Harness: *harness, RunFile: *runFile, Client: client}
		if mode == "reconcile" {
			if *task == "" || *session == "" {
				return errors.New("reconcile requires --task and --session")
			}
			return worker.Reconcile(ctx, *task, *session)
		}
		return worker.Run(ctx)
	}
	linearToken := os.Getenv("LINEAR_ACCESS_TOKEN")
	if linearToken == "" {
		return errors.New("LINEAR_ACCESS_TOKEN must be an installed app OAuth token")
	}
	publisher := &linearbridge.Linear{Token: linearToken, Client: &http.Client{Timeout: 8 * time.Second, CheckRedirect: client.CheckRedirect}}
	bridge := &linearbridge.Server{Store: store, WorkerToken: token, WebhookSecret: os.Getenv("LINEAR_WEBHOOK_SECRET"), OrganizationID: os.Getenv("LINEAR_ORGANIZATION_ID"), TeamID: os.Getenv("LINEAR_TEAM_ID"), Linear: publisher}
	handler, err := bridge.Handler()
	if err != nil {
		return err
	}
	server := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	go bridge.RunOutbox(ctx)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	slog.Info("Linear bridge coordinator listening", "address", *listen)
	if err = server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
