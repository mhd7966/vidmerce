// Command api is the HTTP entrypoint for the Vidmerce platform. The actual
// wiring lives in internal/platform/app; this file's only job is to load
// config, build the Application, run it, and ensure resources are closed on
// the way out.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/mhd7966/vidmerce/internal/platform/app"
	"github.com/mhd7966/vidmerce/internal/platform/config"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(".env")
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx := context.Background()
	a, err := app.New(ctx, cfg)
	if err != nil {
		return fmt.Errorf("build app: %w", err)
	}
	defer a.Close()

	return a.Run(ctx)
}
