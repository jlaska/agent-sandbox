package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/jlaska/agent-sandbox/internal/agentrun"
)

// exitInterrupted is the conventional 128+SIGINT status.
const exitInterrupted = 130

func main() {
	cfg, err := agentrun.ParseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// SIGINT/SIGTERM cancel the context instead of killing the process, so
	// Run's cleanup (sandbox delete, token revoke, provider delete) still runs.
	// Signals stay captured until exit: a second Ctrl-C during cleanup is
	// ignored on purpose, because killing cleanup would leave the sandbox
	// running with bootstrap egress and a live token.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err = agentrun.RunContext(ctx, cfg)
	stop()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		if errors.Is(err, agentrun.ErrInterrupted) {
			os.Exit(exitInterrupted)
		}
		os.Exit(1)
	}
}
