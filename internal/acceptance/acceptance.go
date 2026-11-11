// Package acceptance runs reproducible, headless checks against the Harness.
// It owns private runtime evidence, temporary credentials, and isolated live
// resources; callers supply options and a destination for progress messages.
package acceptance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Options selects the checks and their read-only inputs. Empty Mode, ConfigDir,
// ReportPath, and ClassifierTimeout use the same defaults as harness-verify.
// RootDir defaults to the nearest enclosing directory containing go.mod.
// Relative paths are resolved from RootDir. Runtime evidence is retained under
// RootDir/.runtime/harness-validation; temporary API credentials are removed.
type Options struct {
	Mode              string
	Only              string
	RootDir           string
	ConfigDir         string
	ReportPath        string
	MilvusURL         string
	CorpusDir         string
	ClassifierTimeout time.Duration
}

type verifier struct {
	report                     report
	root, work, config, milvus string
	classifierTimeout          time.Duration
	only                       string
	output                     io.Writer
	outputErr                  error
}

// Run executes selected checks and writes JSON and Markdown reports, including
// failed and canceled checks. A failed check returns an error after its report
// is saved; cancellation remains discoverable with errors.Is. Live checks only
// mutate independent workspaces, databases, and collections created by this run.
// A nil output discards progress messages. Invalid options create no evidence.
func Run(ctx context.Context, opts Options, output io.Writer) error {
	if err := opts.resolve(); err != nil {
		return err
	}
	if output == nil {
		output = io.Discard
	}
	base := filepath.Join(opts.RootDir, ".runtime", "harness-validation")
	if err := os.MkdirAll(base, 0700); err != nil {
		return err
	}
	work, err := os.MkdirTemp(base, "run-")
	if err != nil {
		return err
	}
	v := &verifier{
		only: opts.Only, root: opts.RootDir, work: work,
		config: opts.ConfigDir, milvus: opts.MilvusURL,
		classifierTimeout: opts.ClassifierTimeout, output: output,
		report: report{Version: 1, StartedAt: time.Now().UTC(), GoVersion: runtime.Version(), Mode: opts.Mode, Success: true},
	}
	if opts.Mode == "offline" || opts.Mode == "all" {
		v.offline(ctx)
	}
	if opts.Mode == "live" || opts.Mode == "all" {
		v.live(ctx)
	}
	if opts.CorpusDir != "" {
		v.run(ctx, "corpus_snapshot", "offline", func() (map[string]any, error) { return verifyCorpus(ctx, opts.CorpusDir) })
	}
	if err := v.writeReport(opts.ReportPath); err != nil {
		return errors.Join(err, v.outputErr)
	}
	v.printf("Report: %s\n", opts.ReportPath)
	var result error
	if !v.report.Success {
		result = errors.New("acceptance checks failed; see report")
	}
	return errors.Join(result, ctx.Err(), v.outputErr)
}

func (o *Options) resolve() error {
	if o.Mode == "" {
		o.Mode = "offline"
	}
	if o.ClassifierTimeout == 0 {
		o.ClassifierTimeout = 15 * time.Second
	}
	if (o.Mode != "offline" && o.Mode != "live" && o.Mode != "all") || o.ClassifierTimeout <= 0 || o.ClassifierTimeout > 30*time.Second {
		return errors.New("invalid verification arguments")
	}
	matched := o.Only == "" || o.Only == "corpus_snapshot" && o.CorpusDir != ""
	if o.Mode != "live" && o.Only == "offline_regression" {
		matched = true
	}
	if o.Mode != "offline" {
		for _, name := range liveCheckNames {
			matched = matched || o.Only == name
		}
	}
	if !matched {
		return errors.New("no checks matched -only")
	}
	if o.RootDir == "" {
		root, err := os.Getwd()
		if err != nil {
			return err
		}
		for {
			if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
				break
			}
			parent := filepath.Dir(root)
			if parent == root {
				return errors.New("run inside the LanAgent repository")
			}
			root = parent
		}
		o.RootDir = root
	}
	var err error
	if o.RootDir, err = filepath.Abs(o.RootDir); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(o.RootDir, "go.mod")); err != nil {
		return fmt.Errorf("repository go.mod: %w", err)
	}
	if o.ConfigDir == "" {
		o.ConfigDir = "configs/interview"
	}
	if o.ReportPath == "" {
		o.ReportPath = ".runtime/harness-validation/report.json"
	}
	for _, path := range []*string{&o.ConfigDir, &o.ReportPath, &o.CorpusDir} {
		if *path != "" && !filepath.IsAbs(*path) {
			*path = filepath.Join(o.RootDir, *path)
		}
	}
	return nil
}

func (v *verifier) run(ctx context.Context, name, mode string, fn func() (map[string]any, error)) {
	if v.only != "" && name != v.only {
		return
	}
	row := check{Name: name, Mode: mode, Status: "not_run"}
	if ctx.Err() != nil {
		row.Error = "verification canceled before this scenario"
	} else {
		start := time.Now()
		var err error
		row.Metrics, err = fn()
		row.DurationMS = float64(time.Since(start)) / float64(time.Millisecond)
		row.Status = "passed"
		if err != nil {
			row.Status, row.Error = "failed", err.Error()
		}
	}
	if row.Status != "passed" {
		v.report.Success = false
	}
	v.report.Checks = append(v.report.Checks, row)
	v.printf("%s: %s\n", name, row.Status)
	if row.Error != "" {
		v.printf("  %s\n", row.Error)
	}
}

func (v *verifier) printf(format string, args ...any) {
	if v.outputErr == nil {
		_, v.outputErr = fmt.Fprintf(v.output, format, args...)
	}
}
