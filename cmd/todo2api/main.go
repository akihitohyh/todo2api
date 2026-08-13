package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"todo2api/internal/admin"
	"todo2api/internal/config"
	"todo2api/internal/gateway"
	"todo2api/internal/pool"
	"todo2api/internal/session"
	"todo2api/internal/storage"
	"todo2api/internal/transport"
	webui "todo2api/web"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	masterKey, err := cfg.MasterKey()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	store, err := storage.Open(ctx, cfg, masterKey)
	if err != nil {
		log.Fatalf("storage: %v", err)
	}
	defer store.Close()
	keys, err := store.PoolKeys(ctx)
	if err != nil {
		log.Fatalf("load accounts: %v", err)
	}
	cfg.Pool.Keys = keys
	p, err := pool.New(cfg, store)
	if err != nil {
		log.Fatalf("pool: %v", err)
	}
	for _, warning := range p.Warnings() {
		log.Printf("pool warning: %v", warning)
	}
	log.Printf("initialized %d of %d upstream accounts", p.Len(), p.Configured())
	log.Printf("discovered %d common upstream models", len(p.Models()))

	sess := session.New()
	sess.StartCleanupContext(ctx, 5*time.Minute)
	adminService := admin.New(cfg, store, p, ctx)
	p.SetRepository(adminService)
	warmDone := make(chan struct{})
	go func() {
		defer close(warmDone)
		p.Warm(ctx, func(ready, skipped, processed int) {
			if processed == p.Configured() || processed%50 < 2 {
				log.Printf("account pool warmup: %d ready, %d skipped, %d configured", ready, skipped, p.Configured())
			}
		})
	}()
	gw := gateway.New(cfg, p, sess, adminService)
	srv := transport.New(cfg, gw)
	adminDone := make(chan struct{})
	go func() {
		defer close(adminDone)
		select {
		case <-warmDone:
			adminService.Run(ctx)
		case <-ctx.Done():
		}
	}()
	mux := http.NewServeMux()
	srv.Register(mux)
	adminService.Register(mux)
	mux.Handle("/", webui.Handler())

	log.Printf("todo2api listening on %s", cfg.Server.Addr)

	server := &http.Server{
		Addr:           cfg.Server.Addr,
		Handler:        mux,
		ReadTimeout:    15 * time.Second,
		WriteTimeout:   6 * time.Minute,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("http shutdown: %v", err)
			_ = server.Close()
		}
	}()
	serveErr := server.ListenAndServe()
	if serveErr != nil && serveErr != http.ErrServerClosed {
		log.Printf("http server: %v", serveErr)
		stop()
	}
	<-shutdownDone
	<-adminDone
	adminService.Wait()
	<-warmDone
}
