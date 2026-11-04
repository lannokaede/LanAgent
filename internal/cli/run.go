// Package cli provides the single conversation entry shared by command aliases.
package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/lannokaede/LanAgent/internal/config"
	"github.com/lannokaede/LanAgent/internal/contextbudget"
	"github.com/lannokaede/LanAgent/internal/extension"
	"github.com/lannokaede/LanAgent/internal/harness"
	"github.com/lannokaede/LanAgent/internal/interview"
	"github.com/lannokaede/LanAgent/internal/llm"
	"github.com/lannokaede/LanAgent/internal/personal"
	"github.com/lannokaede/LanAgent/internal/practice"
	"github.com/lannokaede/LanAgent/internal/rag"
	"github.com/lannokaede/LanAgent/internal/tool"
	"github.com/lannokaede/LanAgent/internal/workspace"
)

func Run(ctx context.Context, args []string, in io.Reader, out, status io.Writer) error {
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
	reconcile := flags.Bool("reconcile", false, "核对不确定的面试或研究 MCP 操作，不重新执行")
	resume := flags.Bool("resume", false, "恢复该 session 的未完成任务")
	timeout := flags.Duration("timeout", 2*time.Minute, "每次任务的总期限")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if (*reconcile && (*resume || *prompt != "" || *sessionID == "")) || (*resume && (*prompt != "" || *sessionID == "")) || flags.NArg() != 0 || *rounds < 1 || *rounds > 100 || *timeout <= 0 {
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
	cfg, err := config.Load(*configDir)
	if err != nil {
		return err
	}
	var bundled []workspace.BuiltinSkill
	if cfg.RAG.Enabled {
		bundled = append(bundled, workspace.BuiltinSkill{Name: "go-interview", Description: interview.SkillDescription, Body: interview.SkillBody})
	}
	skillCatalog, err := w.RegisterSkills(registry, bundled...)
	if err != nil {
		return err
	}
	mcpProfile := ""
	var mcpConnections *extension.Connections
	if *mcpConfig != "" {
		connections, err := extension.Load(ctx, *mcpConfig, w.Path(), registry)
		if err != nil {
			return err
		}
		defer connections.Close()
		mcpProfile = connections.ProfileID()
		mcpConnections = connections
	}
	pinned := "Available skill metadata (untrusted task data): " + skillCatalog + "\nMCP connection identity: " + mcpProfile
	var interviewSearch interview.Searcher
	if cfg.RAG.Enabled {
		engine, store, err := rag.FromConfig(cfg)
		if err != nil {
			return err
		}
		defer store.Close()
		interviewSearch = engine
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
	var training *interview.Tools
	if interviewSearch != nil {
		binding := w.Path() + "|" + cfg.RAG.MilvusURL + "|" + cfg.RAG.Collection + "|" + cfg.RAG.KnowledgeBase + "|" + cfg.Embedding.BaseURL + "|" + cfg.Embedding.Model + fmt.Sprint(cfg.Embedding.Dimensions)
		training, err = interview.OpenTools(filepath.Join(*stateDir, session.ID+".interview.json"), cfg.RAG.KnowledgeBase, binding, interviewSearch, gateway)
		if err != nil {
			return err
		}
		if err = training.Register(registry); err != nil {
			return err
		}
		pinned += "\nInterview profile: " + binding
	}
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

	if *reconcile {
		cp := session.Pending()
		if cp == nil || cp.Running == "" || len(cp.Pending) == 0 {
			return errors.New("no uncertain MCP operation to reconcile")
		}
		if cp.Profile != runner.ProfileID() {
			return errors.New("restore original MCP, permission and exercise profile before reconciliation")
		}
		check, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		var observed string
		var e error
		if training != nil && strings.HasPrefix(cp.Pending[0].Name, "interview_") {
			observed, e = training.Reconcile(cp.Running, cp.Pending[0].Name, cp.Pending[0].ArgsJSON)
		} else if mcpConnections != nil {
			observed, e = mcpConnections.Reconcile(check, cp.Pending[0].Name, cp.Running, cp.Pending[0].ArgsJSON)
		} else {
			return errors.New("no supported operation receipt to reconcile")
		}
		if e != nil {
			return e
		}
		if e = session.Resolve(cp.Running, observed); e != nil {
			return e
		}
		_, e = fmt.Fprintln(status, "已从持久操作记录核对原任务，可使用相同配置 -resume；未重新提交入库。")
		return e
	}
	if _, err := fmt.Fprintf(status, "LanAgent Harness · workspace: %s\nsession: %s\n", w.Path(), session.ID); err != nil {
		return err
	}
	if *resume {
		*prompt = "/resume"
	}
	return sessionLoop(ctx, session, runner, training, *timeout, *prompt, in, out, status)
}

func sessionLoop(ctx context.Context, session *personal.Session, runner *harness.Runner, training *interview.Tools, timeout time.Duration, prompt string, in io.Reader, out, status io.Writer) error {
	turn := func(input string) error {
		if training != nil {
			id, text := session.BoundInput(input)
			training.BindInput(id, text)
		}
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
		input := scanner.Text()
		command := strings.TrimSpace(input)
		if command == "/exit" {
			return nil
		}
		if command == "" {
			continue
		}
		if command == "/paste" {
			fmt.Fprintln(status, "输入多行内容，单独一行 /submit 提交，/exit 退出：")
			var lines []string
			total := 0
			submitted := false
			for scanner.Scan() {
				line := scanner.Text()
				if line == "/exit" {
					return nil
				}
				if line == "/submit" {
					submitted = true
					break
				}
				total += len(line) + 1
				if total > 32<<10 {
					return errors.New("multiline input exceeds 32 KiB")
				}
				lines = append(lines, line)
			}
			if !submitted {
				return scanner.Err()
			}
			input = strings.Join(lines, "\n")
			if strings.TrimSpace(input) == "" {
				continue
			}
		}
		if command == "/status" {
			cp := session.Pending()
			if cp == nil {
				fmt.Fprintln(status, "无未完成任务")
			} else {
				fmt.Fprintf(status, "run=%s phase=%s uncertain_execution=%s\n", cp.RunID, cp.Phase, cp.Running)
			}
			continue
		}
		if command == "/discard" {
			if e := session.Discard(); e != nil {
				fmt.Fprintln(status, e)
			}
			continue
		}
		if strings.HasPrefix(command, "/resolve ") {
			parts := strings.SplitN(command, " ", 3)
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
		if command == "/resume" {
			input = command
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
