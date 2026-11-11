// Package application assembles one local conversation and owns its resources.
// It contains no terminal protocol. Callers serialize Turn and recovery actions,
// wait for a turn to return, then Close before opening the next conversation.
package application

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/lannokaede/LanAgent/internal/config"
	"github.com/lannokaede/LanAgent/internal/contextbudget"
	"github.com/lannokaede/LanAgent/internal/extension"
	"github.com/lannokaede/LanAgent/internal/harness"
	"github.com/lannokaede/LanAgent/internal/interview"
	"github.com/lannokaede/LanAgent/internal/llm"
	"github.com/lannokaede/LanAgent/internal/observability/telemetry"
	"github.com/lannokaede/LanAgent/internal/personal"
	"github.com/lannokaede/LanAgent/internal/personalmemory"
	"github.com/lannokaede/LanAgent/internal/workspace"
)

// Options holds non-secret launch choices. Model credentials stay in config.
// Rounds must be 1..100, Timeout positive, and WebPolicy auto or off.
type Options struct {
	ConfigDir, Workspace, StateDir, SessionID string
	MCPConfig, Exercise, WebPolicy            string
	MemoryDB, MemoryCollection                string
	Rounds                                    int
	Timeout                                   time.Duration
	AllowWrite, AllowCommands, Summary        bool
	Memory, MemoryVectors, MemoryInit         bool
}

func (o Options) validate() error {
	if o.Rounds < 1 || o.Rounds > 100 || o.Timeout <= 0 {
		return errors.New("rounds must be 1–100 and timeout must be positive")
	}
	if o.WebPolicy != "auto" && o.WebPolicy != "off" {
		return errors.New("web-policy must be auto or off")
	}
	if (o.MemoryVectors && !o.Memory) || (o.MemoryInit && !o.MemoryVectors) {
		return errors.New("memory vectors require -memory; memory init requires -memory-vectors")
	}
	return nil
}

// Runtime owns one session, model clients, tool connections, and optional memory
// synchronization. No acquired handle escapes to the terminal adapter.
type Runtime struct {
	resources resources
	session   *personal.Session
	runner    *harness.Runner
	training  *interview.Tools
	mcp       *extension.Connections
	memory    *personalmemory.Service
	workspace string
	startSync func()
}

type Info struct{ Workspace, SessionID string }

func (r *Runtime) Info() Info { return Info{r.workspace, r.session.ID} }

// Close is idempotent. It stops and joins workers before closing their stores,
// then flushes telemetry under a fresh bounded context, even after cancellation.
func (r *Runtime) Close() error { return r.resources.close() }

func Open(ctx context.Context, o Options) (_ *Runtime, err error) {
	if err = o.validate(); err != nil {
		return nil, err
	}
	r := &Runtime{}
	defer func() {
		if err != nil {
			err = errors.Join(err, r.Close())
		}
	}()
	cfg, err := config.Load(o.ConfigDir)
	if err != nil {
		return nil, err
	}
	shutdown, err := telemetry.Init(ctx, cfg.Telemetry)
	if err != nil {
		return nil, err
	}
	r.resources.add(func() error {
		finish, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return shutdown(finish)
	})
	w, err := workspace.Open(o.Workspace)
	if err != nil {
		return nil, err
	}
	r.resources.add(w.Close)
	r.workspace = w.Path()
	if err = resolveStatePaths(&o); err != nil {
		return nil, err
	}
	registry, err := w.Registry()
	if err != nil {
		return nil, err
	}
	if err = w.RegisterActions(registry, o.AllowWrite, o.AllowCommands); err != nil {
		return nil, err
	}
	pinned, search, err := r.registerKnowledge(ctx, cfg, o, w, registry)
	if err != nil {
		return nil, err
	}
	gateway, classifier, err := r.openModels(cfg)
	if err != nil {
		return nil, err
	}
	r.runner = harness.NewRunner(gateway, registry, o.Rounds).
		WithRouting(cfg.Routing.Mode, classifier, cfg.Routing.Timeout).
		WithBudget(contextbudget.Policy{Window: cfg.Context.Window, OutputReserve: cfg.Context.OutputReserve, ToolReserve: cfg.Context.ToolReserve, SummaryTokens: cfg.Context.SummaryTokens}).
		WithToolPolicy(harness.ToolPolicy{MaxCalls: 20, MaxArgsBytes: 128 << 10, MaxResultBytes: 256 << 10, Timeout: 65 * time.Second})
	if o.Summary {
		r.runner.WithSummary(gateway)
	}
	r.session, err = personal.Open(o.StateDir, o.SessionID, w.Path())
	if err != nil {
		return nil, err
	}
	r.resources.add(r.session.Close)
	if err = r.openMemory(ctx, cfg, o); err != nil {
		return nil, err
	}
	if err = r.registerPractice(cfg, o, registry, search, gateway, &pinned); err != nil {
		return nil, err
	}
	r.runner.WithPinnedState(func() string { return pinned })
	if cp := r.session.Pending(); cp == nil || cp.Profile == r.runner.ProfileID() {
		if err = r.session.SaveLaunch(launch(o, w.Path())); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func resolveStatePaths(o *Options) (err error) {
	if o.StateDir == "" {
		o.StateDir, err = personal.DefaultStateDir()
		if err != nil {
			return err
		}
	}
	if o.MemoryDB == "" {
		o.MemoryDB = filepath.Join(o.StateDir, "personal-memory.db")
	}
	return nil
}

func launch(o Options, work string) personal.Launch {
	abs := func(path string) string {
		if path != "" {
			if p, err := filepath.Abs(path); err == nil {
				return p
			}
		}
		return path
	}
	memory := o.Memory
	return personal.Launch{
		WebPolicy: o.WebPolicy, Config: abs(o.ConfigDir), Workspace: work,
		MCP: abs(o.MCPConfig), Exercise: o.Exercise,
		Write: o.AllowWrite, Commands: o.AllowCommands, Summary: o.Summary,
		Rounds: o.Rounds, Timeout: o.Timeout, Memory: &memory,
		MemoryDB: abs(o.MemoryDB), MemoryVectors: o.MemoryVectors, MemoryCollection: o.MemoryCollection,
	}
}

// Turn retains real input binding and applies session persistence to normal and
// resumed runs. Memory commands operate on SQLite without a model invocation.
// The caller supplies the task deadline; it also bounds gateway retries/tools.
func (r *Runtime) Turn(ctx context.Context, input string, emit func(harness.Event) error) (*llm.ChatResponse, error) {
	if r.training != nil {
		id, text := r.session.BoundInput(input)
		r.training.BindInput(id, text)
	}
	if personalmemory.IsCommand(input) {
		if r.memory == nil {
			return nil, errors.New("personal memory disabled; enable -memory")
		}
		var output bytes.Buffer
		if _, err := r.memory.Command(ctx, input, "session:"+r.session.ID, &output); err != nil {
			return nil, err
		}
		return &llm.ChatResponse{Content: strings.TrimSuffix(output.String(), "\n")}, nil
	}
	if r.startSync != nil {
		r.startSync()
	}
	if input == "/resume" {
		return r.session.Resume(ctx, r.runner, emit)
	}
	return r.session.Turn(ctx, r.runner, input, emit)
}

func (r *Runtime) Pending() *harness.Checkpoint    { return r.session.Pending() }
func (r *Runtime) Discard() error                  { return r.session.Discard() }
func (r *Runtime) Resolve(id, result string) error { return r.session.Resolve(id, result) }

// Reconcile checks an existing receipt under the original capability profile;
// it never submits an uncertain operation again.
func (r *Runtime) Reconcile(ctx context.Context) error {
	cp := r.session.Pending()
	if cp == nil || cp.Running == "" || len(cp.Pending) == 0 {
		return errors.New("no uncertain MCP operation to reconcile")
	}
	if cp.Profile != r.runner.ProfileID() {
		return errors.New("restore original MCP, permission and exercise profile before reconciliation")
	}
	var observed string
	var err error
	if r.training != nil && strings.HasPrefix(cp.Pending[0].Name, "interview_") {
		observed, err = r.training.Reconcile(cp.Running, cp.Pending[0].Name, cp.Pending[0].ArgsJSON)
	} else if r.mcp != nil {
		observed, err = r.mcp.Reconcile(ctx, cp.Pending[0].Name, cp.Running, cp.Pending[0].ArgsJSON)
	} else {
		return errors.New("no supported operation receipt to reconcile")
	}
	if err != nil {
		return err
	}
	return r.session.Resolve(cp.Running, observed)
}
