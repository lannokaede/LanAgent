// Package cli provides terminal parsing and presentation for conversation commands.
package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/lannokaede/LanAgent/internal/application"
	"github.com/lannokaede/LanAgent/internal/harness"
	"github.com/lannokaede/LanAgent/internal/personalmemory"
)

func runOnce(ctx context.Context, args []string, in io.Reader, out, status io.Writer) (err error) {
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
	webPolicy := flags.String("web-policy", "auto", "网页工具策略：auto 按需调用；off 禁止 MCP web_search/web_fetch（不关闭模型等其他网络连接）")
	mcpConfig := flags.String("mcp-config", "", "MCP 服务器 JSON 配置；只加载 tools 白名单")
	summary := flags.Bool("summary", true, "超出预算时生成历史摘要，失败回退裁剪")
	exercise := flags.String("exercise", "", "绑定固定 Go 编程题，格式 id/version；启用 Docker 工具")
	reconcile := flags.Bool("reconcile", false, "核对不确定的面试或研究 MCP 操作，不重新执行")
	resume := flags.Bool("resume", false, "恢复该 session 的未完成任务")
	memoryEnabled := flags.Bool("memory", true, "启用本地 SQLite 个人记忆；用 /remember 显式保存")
	memoryDB := flags.String("memory-db", "", "个人记忆数据库；默认 state-dir/personal-memory.db")
	memoryVectors := flags.Bool("memory-vectors", false, "后台同步个人记忆到独立 Milvus 集合并启用语义召回")
	memoryInit := flags.Bool("memory-init", false, "显式初始化个人记忆 Milvus 集合；需 -memory-vectors")
	memoryCollection := flags.String("memory-collection", "lanagent_personal_memory_v1", "独立个人记忆集合，不使用知识库/Mem0 集合")
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
	if *webPolicy != "auto" && *webPolicy != "off" {
		return errors.New("web-policy must be auto or off")
	}
	if (*memoryVectors && !*memoryEnabled) || (*memoryInit && !*memoryVectors) {
		return errors.New("memory vectors require -memory; memory init requires -memory-vectors")
	}
	o := application.Options{
		ConfigDir: *configDir, Workspace: *workDir, StateDir: *stateDir, SessionID: *sessionID,
		MCPConfig: *mcpConfig, Exercise: *exercise, WebPolicy: *webPolicy,
		MemoryDB: *memoryDB, MemoryCollection: *memoryCollection,
		Rounds: *rounds, Timeout: *timeout, Summary: *summary,
		AllowWrite: *allowWrite, AllowCommands: *allowCommands,
		Memory: *memoryEnabled, MemoryVectors: *memoryVectors, MemoryInit: *memoryInit,
	}
	if personalmemory.IsCommand(*prompt) && !*memoryVectors {
		call, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		return application.MemoryCommand(call, o, *prompt, out)
	}
	runtime, err := application.Open(ctx, o)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := runtime.Close(); closeErr != nil {
			if errors.Is(err, errMenu) {
				err = closeErr
			} else {
				err = errors.Join(err, closeErr)
			}
		}
	}()
	if *reconcile {
		check, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err = runtime.Reconcile(check); err != nil {
			return err
		}
		_, err = fmt.Fprintln(status, "已从持久操作记录核对原任务，可使用相同配置 -resume；未重新提交入库。")
		return err
	}
	info := runtime.Info()
	if _, err := fmt.Fprintf(status, "LanAgent Harness · workspace: %s\nsession: %s\n", info.Workspace, info.SessionID); err != nil {
		return err
	}
	if *prompt == "" && !*resume {
		fmt.Fprintln(status, "/menu 会话管理 · /help 帮助 · /paste 多行输入 · /exit 结束对话")
	}
	if *resume {
		*prompt = "/resume"
	}
	return sessionLoop(ctx, runtime, *timeout, *prompt, in, out, status)
}

func sessionLoop(ctx context.Context, runtime *application.Runtime, timeout time.Duration, prompt string, in io.Reader, out, status io.Writer) error {
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
		response, err := runtime.Turn(call, input, eventSink)
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
		if command == "/menu" || command == "/sessions" {
			return errMenu
		}
		if command == "/help" {
			fmt.Fprintln(status, "/menu 会话菜单 · /sessions 选择会话 · /status 未完成任务 · /resume 恢复 · /paste 多行输入 · /remember 保存记忆 · /memory 查看记忆 · /forget 删除记忆 · /exit 退出对话")
			continue
		}
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
			cp := runtime.Pending()
			if cp == nil {
				fmt.Fprintln(status, "无未完成任务")
			} else {
				fmt.Fprintf(status, "run=%s phase=%s uncertain_execution=%s\n", cp.RunID, cp.Phase, cp.Running)
			}
			continue
		}
		if command == "/discard" {
			if e := runtime.Discard(); e != nil {
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
			if e := runtime.Resolve(parts[1], parts[2]); e != nil {
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
