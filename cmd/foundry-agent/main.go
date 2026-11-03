// Command foundry-agent runs on every Foundry device.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"foundry.dev/foundry/internal/agent"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: foundry-agent run|version")
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	switch os.Args[1] {
	case "version":
		fmt.Println(version)
	case "run":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		cfg, err := agent.LoadConfig("/usr/lib/foundry/agent.json", "/etc/foundry/agent.json")
		if err != nil {
			log.Error("config", "err", err)
			os.Exit(1)
		}
		if err := agent.Run(ctx, cfg, log); err != nil {
			log.Error("agent", "err", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown command", os.Args[1])
		os.Exit(2)
	}
}
