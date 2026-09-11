package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/kyosu-1/tetherd/internal/agent"
	"github.com/kyosu-1/tetherd/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("tetherd-agent", version.Version)
		return
	}
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.SetPrefix("tetherd-agent ")
	cfg, err := agent.ConfigFromEnv(os.Getenv)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := agent.New(cfg, log.Printf).ListenAndServe(ctx); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
