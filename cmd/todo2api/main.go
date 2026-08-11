package main

import (
	"flag"
	"log"
	"net/http"

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
	log.Printf("discovered %d common upstream models", len(p.Models()))

	sess := session.New()
	gw := gateway.New(cfg, p, sess)
	srv := transport.New(cfg, gw)

	log.Printf("todo2api listening on %s", cfg.Server.Addr)
	if err := http.ListenAndServe(cfg.Server.Addr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}
