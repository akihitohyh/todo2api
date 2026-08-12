package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"time"

	"todo2api/internal/config"
	"todo2api/internal/gateway"
	"todo2api/internal/pool"
	"todo2api/internal/session"
	"todo2api/internal/transport"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	p, err := pool.New(cfg)
	if err != nil {
		log.Fatalf("pool: %v", err)
	}
	for _, warning := range p.Warnings() {
		log.Printf("pool warning: %v", warning)
	}
	log.Printf("initialized %d of %d upstream accounts", p.Len(), p.Configured())
	log.Printf("discovered %d common upstream models", len(p.Models()))

	bg := context.Background()
	go p.Warm(bg, func(ready, skipped, processed int) {
		if processed == p.Configured() || processed%50 < 2 {
			log.Printf("account pool warmup: %d ready, %d skipped, %d configured", ready, skipped, p.Configured())
		}
	})
	go p.WatchKeyFiles(bg, cfg, log.Printf)

	sess := session.New()
	sess.StartCleanup(5 * time.Minute)
	gw := gateway.New(cfg, p, sess)
	srv := transport.New(cfg, gw)

	log.Printf("todo2api listening on %s", cfg.Server.Addr)

	server := &http.Server{
		Addr:           cfg.Server.Addr,
		Handler:        srv.Handler(),
		ReadTimeout:    15 * time.Second,
		WriteTimeout:   6 * time.Minute,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
