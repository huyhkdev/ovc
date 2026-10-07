package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ovc/internal/catalog"
	"ovc/internal/config"
	"ovc/internal/githost"
	"ovc/internal/gitops"
	"ovc/internal/jobs"
	"ovc/internal/server"
	"ovc/internal/store"
)

func main() {
	cfgPath := flag.String("config", "ovc-server.yaml", "path to config file")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(*cfgPath, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(cfgPath string, log *slog.Logger) error {
	cfg, err := config.LoadServer(cfgPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	token := ""
	if cfg.Git.TokenEnv != "" {
		token = os.Getenv(cfg.Git.TokenEnv)
	}
	mirror, err := gitops.Open(ctx, cfg.Storage.MirrorDir, gitops.Config{Remote: cfg.Git.Remote, Token: token})
	if err != nil {
		return err
	}
	st, err := store.OpenFile(cfg.Storage.StateFile)
	if err != nil {
		return err
	}
	host, err := githost.New(cfg.Git.Host, cfg.Git.APIURL, cfg.Git.Repo, cfg.Git.Remote, token)
	if err != nil {
		return err
	}
	jm := jobs.New()
	h := server.New(server.Deps{
		Cfg: cfg, Git: mirror, Store: st, Jobs: jm, Log: log, BaseCtx: ctx,
		Catalog: catalog.Oracle{DBs: cfg.Databases}, Host: host,
	})
	srv := &http.Server{Addr: cfg.Listen, Handler: h, ReadHeaderTimeout: 10 * time.Second}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("ovc-server listening", "addr", cfg.Listen, "remote", cfg.Git.Remote, "envs", cfg.Envs)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	jm.Wait()
	return nil
}
