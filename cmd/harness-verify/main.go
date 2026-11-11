// harness-verify runs reproducible, headless acceptance checks. Live checks use
// isolated workspaces, databases and uniquely named collections, never user data.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/lannokaede/LanAgent/internal/acceptance"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output, status io.Writer) error {
	var opts acceptance.Options
	f := flag.NewFlagSet("harness-verify", flag.ContinueOnError)
	f.SetOutput(status)
	f.StringVar(&opts.Mode, "mode", "offline", "offline, live or all")
	f.StringVar(&opts.Only, "only", "", "run one named check")
	f.StringVar(&opts.ConfigDir, "config", "configs/interview", "existing API/embedding configuration, read only")
	f.StringVar(&opts.ReportPath, "report", ".runtime/harness-validation/report.json", "JSON report path")
	f.StringVar(&opts.MilvusURL, "milvus-url", "", "override Milvus for isolated live collections only")
	f.StringVar(&opts.CorpusDir, "corpus-dir", "", "optional existing corpus snapshot to verify counts and checksums")
	f.DurationVar(&opts.ClassifierTimeout, "classifier-timeout", 15*time.Second, "live classification budget, recorded in report")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || opts.ClassifierTimeout <= 0 {
		return errors.New("invalid verification arguments")
	}
	return acceptance.Run(ctx, opts, output)
}
