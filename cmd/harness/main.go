package main

import (
	"context"
	"fmt"
	"github.com/lannokaede/LanAgent/internal/cli"
	"io"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); _ = os.Stdin.Close() }()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string, in io.Reader, out, status io.Writer) error {
	return cli.Run(ctx, args, in, out, status)
}
