// vpcctl operates the public networking API and prepares native integration.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"globalvpc.io/controller/internal/vpcctl"
)

var (
	version    = "v0.1.0"
	commit     = "unknown"
	sourceDate = "unknown"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	app := vpcctl.App{Out: os.Stdout, ErrOut: os.Stderr, Version: version, Commit: commit, SourceDate: sourceDate}
	if err := app.Run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
