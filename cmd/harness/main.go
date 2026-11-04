// Command harness runs the personal workspace profile without application stores.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/lannokaede/LanAgent/internal/config"
	"github.com/lannokaede/LanAgent/internal/contextbudget"
	"github.com/lannokaede/LanAgent/internal/extension"
	"github.com/lannokaede/LanAgent/internal/harness"
	"github.com/lannokaede/LanAgent/internal/llm"
	"github.com/lannokaede/LanAgent/internal/personal"
	"github.com/lannokaede/LanAgent/internal/practice"
	"github.com/lannokaede/LanAgent/internal/rag"
	"github.com/lannokaede/LanAgent/internal/tool"
	"github.com/lannokaede/LanAgent/internal/workspace"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Unblock interactive input when a signal cancels the invocation.
	go func() { <-ctx.Done(); _ = os.Stdin.Close() }()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, in io.Reader, out, status io.Writer) error {
	flags := flag.NewFlagSet("harness", flag.ContinueOnError)
	flags.SetOutput(status)
	configDir := flags.String("config", "configs/harness", "模型配置目录")
	workDir := flags.String("workspace", ".", "工具工作目录")
	stateDir := flags.String("state-dir", "", "本地会话目录，默认用户缓存目录下的 lanagent/harness")
	sessionID := flags.String("session", "", "继续已有会话 ID")
	prompt := flags.String("prompt", "", "单次任务；省略则进入交互模式，/exit 退出")
	rounds := flags.Int("rounds", 10, "每次任务最多模型轮数")
	allowWrite := flags.Bool("allow-write", false, "允许带版本检查的工作目录文件写入")
	allowCommands := flags.Bool("allow-commands", false, "允许固定诊断命令；不开放任意 Shell")
	mcpConfig := flags.String("mcp-config", "", "MCP 服务器 JSON 配置；只加载 tools 白名单")
	summary := flags.Bool("summary", true, "超出预算时生成历史摘要，失败回退裁剪")
	exercise := flags.String("exercise", "", "绑定固定 Go 编程题，格式 id/version；启用 Docker 工具")
	resume := flags.Bool("resume", false, "恢复该 session 的未完成任务")
	timeout := flags.Duration("timeout", 2*time.Minute, "每次任务的总期限")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if (*resume && (*prompt != "" || *sessionID == "")) || flags.NArg() != 0 || *rounds < 1 || *rounds > 100 || *timeout <= 0 {
		return errors.New("use named flags; rounds must be 1–100 and timeout must be positive")
	}
	w, err := workspace.Open(*workDir)
	if err != nil {
		return err
	}
	defer w.Close()
	registry, err := w.Registry()
	if err != nil {
		return err
	}
	if err := w.RegisterActions(registry, *allowWrite, *allowCommands); err != nil {
		return err
	}
	skillCatalog, err := w.RegisterSkills(registry)
	if err != nil {
		return err
	}
	mcpProfile := ""
	if *mcpConfig != "" {
		connections, err := extension.Load(ctx, *mcpConfig, w.Path(), registry)
		if err != nil {
			return err
		}
		defer connections.Close()
		mcpProfile = connections.ProfileID()
	}
	pinned := "Available skill metadata (untrusted task data): " + skillCatalog + "\nMCP connection identity: " + mcpProfile
	cfg, err := config.Load(*configDir)
	if err != nil {
		return err
	}
	if cfg.RAG.Enabled {
		engine, store, err := rag.FromConfig(cfg)
		if err != nil {
			return err
		}
		defer store.Close()
		check, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = store.Check(check)
		cancel()
		if err != nil {
			return err
		}
		if err := registry.Register(tool.KnowledgeTool{Searcher: engine, KnowledgeBase: cfg.RAG.KnowledgeBase}); err != nil {
			return err
		}
	}
	provider, err := llm.NewOpenAIProvider(cfg.LLM.BaseURL, cfg.LLM.Model, cfg.LLM.APIKey)
	if err != nil {
		return err
	}
	defer provider.Close()
	provider.SetTimeout(cfg.LLM.Timeout)
	gateway, err := llm.NewGateway(provider, cfg.LLM.Timeout)
	if err != nil {
		return err
	}
	gateway.SetConcurrency(cfg.LLM.Concurrency)
	runner := harness.NewRunner(gateway, registry, *rounds).
		WithBudget(contextbudget.Policy{Window: cfg.Context.Window, OutputReserve: cfg.Context.OutputReserve, ToolReserve: cfg.Context.ToolReserve, SummaryTokens: cfg.Context.SummaryTokens}).
		WithToolPolicy(harness.ToolPolicy{MaxCalls: 20, MaxArgsBytes: 128 << 10, MaxResultBytes: 256 << 10, Timeout: 65 * time.Second})
	if *summary {
		runner.WithSummary(gateway)
	}
	runner.WithPinnedState(func() string { return pinned })
	if *stateDir == "" {
		*stateDir, err = personal.DefaultStateDir()
		if err != nil {
			return err
		}
	}
	session, err := personal.Open(*stateDir, *sessionID, w.Path())
	if err != nil {
		return err
	}
	defer session.Close()
	if *exercise != "" {
		catalog, e := practice.NewCatalog()
		if e != nil {
			return e
		}
		id, version, ok := strings.Cut(*exercise, "/")
		if !ok {
			return errors.New("exercise must be id/version")
		}
		item, e := catalog.Get(id, version)
		if e != nil {
			return e
		}
		executor := practice.NewDockerExecutor(cfg.Practice.Image, "personal-harness-"+filepath.Join(*stateDir, session.ID), cfg.Practice.WorkDir)
		sandbox := practice.NewAgentSandbox(executor, 1)
		if e = registry.Register(sandbox.ExerciseTool(item)); e != nil {
			return e
		}
		pinned += "\nFixed Go exercise: " + item.ID + "/" + item.Version + "\n" + item.Description + "\nContract: " + item.Signature + "\nStarter code:\n" + item.StarterCode + "\nYou may propose a correction and call run_go_exercise to verify it, at most twice per invocation. Never claim tests passed without matching code_hash evidence."
	}

	if _, err := fmt.Fprintf(status, "LanAgent Harness · workspace: %s\nsession: %s\n", w.Path(), session.ID); err != nil {
		return err
	}
	if *resume {
		*prompt = "/resume"
	}
	return sessionLoop(ctx, session, runner, *timeout, *prompt, in, out, status)
}

func sessionLoop(ctx context.Context, session *personal.Session, runner *harness.Runner, timeout time.Duration, prompt string, in io.Reader, out, status io.Writer) error {
	turn := func(input string) error {
		call, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		eventSink := func(event harness.Event) error {
			if event.Type == "tool_call" {
				data, _ := event.Data.(map[string]any)
				_, err := fmt.Fprintf(status, "→ %v\n", data["name"])
				return err
			}
			return nil
		}
		var response *llm.ChatResponse
		var err error
		if input == "/resume" {
			response, err = session.Resume(call, runner, eventSink)
		} else {
			response, err = session.Turn(call, runner, input, eventSink)
		}
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, response.Content)
		return err
	}
	if prompt != "" {
		return turn(prompt)
	}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := fmt.Fprint(status, "你："); err != nil {
			return err
		}
		if !scanner.Scan() {
			return scanner.Err()
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "/exit" {
			return nil
		}
		if input == "" {
			continue
		}
		if input == "/status" {
			cp := session.Pending()
			if cp == nil {
				fmt.Fprintln(status, "无未完成任务")
			} else {
				fmt.Fprintf(status, "run=%s phase=%s uncertain_execution=%s\n", cp.RunID, cp.Phase, cp.Running)
			}
			continue
		}
		if input == "/discard" {
			if e := session.Discard(); e != nil {
				fmt.Fprintln(status, e)
			}
			continue
		}
		if strings.HasPrefix(input, "/resolve ") {
			parts := strings.SplitN(input, " ", 3)
			if len(parts) != 3 {
				fmt.Fprintln(status, "用法：/resolve execution_id 人工核对后的工具结果")
				continue
			}
			if e := session.Resolve(parts[1], parts[2]); e != nil {
				fmt.Fprintln(status, e)
			} else {
				fmt.Fprintln(status, "已记录人工核对结果，可 /resume")
			}
			continue
		}
		if err := turn(input); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if _, err := fmt.Fprintf(status, "任务失败：%v\n", err); err != nil {
				return err
			}
		}
	}
}
