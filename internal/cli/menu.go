package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/lannokaede/LanAgent/internal/personal"
	"golang.org/x/term"
)

var errMenu = errors.New("return to session menu")

// Run selects the terminal launcher automatically only for a bare TTY launch.
// Explicit script flags and piped input retain the original non-menu behavior.
func Run(ctx context.Context, args []string, in io.Reader, out, status io.Writer) error {
	menu := false
	filtered := []string{}
	for _, a := range args {
		if a == "-menu" || a == "--menu" {
			menu = true
		} else {
			filtered = append(filtered, a)
		}
	}
	if len(args) == 0 {
		if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
			menu = true
		}
	}
	reader := bufio.NewReader(in)
	if !menu {
		e := runOnce(ctx, args, &lineInput{reader}, out, status)
		if !errors.Is(e, errMenu) {
			return e
		}
	}
	return launcher(ctx, filtered, reader, out, status)
}

// One read stops at a newline. Nested menus never lose lines buffered by a
// conversation Scanner, including when driven through a pipe in tests.
type lineInput struct{ r *bufio.Reader }

func (l *lineInput) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		b, e := l.r.ReadByte()
		if e != nil {
			if n > 0 {
				return n, nil
			}
			return 0, e
		}
		p[n] = b
		n++
		if b == '\n' {
			break
		}
	}
	return n, nil
}
func terminalText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}
func launcher(ctx context.Context, args []string, r *bufio.Reader, out, status io.Writer) error {
	f := flag.NewFlagSet("menu", flag.ContinueOnError)
	f.SetOutput(status)
	p := personal.Launch{Config: "configs/harness", Workspace: ".", Summary: true, Rounds: 10, Timeout: 2 * time.Minute}
	f.StringVar(&p.Config, "config", p.Config, "资源配置目录")
	f.StringVar(&p.Workspace, "workspace", p.Workspace, "工作目录")
	f.StringVar(&p.MCP, "mcp-config", "", "MCP 配置")
	f.StringVar(&p.Exercise, "exercise", "", "绑定代码题")
	f.BoolVar(&p.Write, "allow-write", false, "文件写入")
	f.BoolVar(&p.Commands, "allow-commands", false, "固定诊断")
	f.BoolVar(&p.Summary, "summary", true, "摘要")
	f.IntVar(&p.Rounds, "rounds", 10, "模型轮数")
	f.DurationVar(&p.Timeout, "timeout", p.Timeout, "任务期限")
	p.Memory = f.Bool("memory", true, "个人记忆")
	f.StringVar(&p.MemoryDB, "memory-db", "", "个人记忆数据库")
	f.BoolVar(&p.MemoryVectors, "memory-vectors", false, "记忆向量索引")
	f.StringVar(&p.MemoryCollection, "memory-collection", "lanagent_personal_memory_v1", "个人记忆集合")
	dir := f.String("state-dir", "", "会话目录")
	session := f.String("session", "", "预选会话 ID")
	if e := f.Parse(args); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return nil
		}
		return e
	}
	if f.NArg() != 0 {
		return errors.New("menu accepts named options only")
	}
	if *dir == "" {
		var e error
		*dir, e = personal.DefaultStateDir()
		if e != nil {
			return e
		}
	}
	if len(args) == 0 {
		if recent, err := personal.List(*dir); err == nil && len(recent) > 0 && recent[0].Launch != nil {
			p = *recent[0].Launch
		}
	}
	read := func(prompt string) (string, error) {
		if e := ctx.Err(); e != nil {
			return "", e
		}
		fmt.Fprint(status, prompt)
		line, e := r.ReadString('\n')
		if e != nil && !(e == io.EOF && line != "") {
			return "", e
		}
		if len(line) > 4096 {
			return "", errors.New("menu input too long")
		}
		return strings.TrimSpace(line), nil
	}
	for {
		fmt.Fprintln(status, "\n╭─ LanAgent · 对话与会话管理 ─╮")
		fmt.Fprintf(status, "  工作目录：%s\n  资源配置：%s\n  写文件：%s  固定诊断：%s  MCP：%s\n", terminalText(p.Workspace), terminalText(p.Config), switchText(p.Write), switchText(p.Commands), optionalText(p.MCP))
		fmt.Fprintln(status, "  1  新建对话\n  2  选择历史会话\n  3  启动设置\n  0  退出")
		choice, e := read("选择 [1]：")
		if e == io.EOF {
			return nil
		}
		if e != nil {
			return e
		}
		switch choice {
		case "0", "q":
			return nil
		case "3":
			if e = editLaunch(&p, read, status); e == io.EOF {
				return nil
			}
			if e != nil {
				return e
			}
			continue
		case "2":
			sessions, e := personal.List(*dir)
			if e != nil {
				return e
			}
			if len(sessions) == 0 {
				fmt.Fprintln(status, "暂无历史会话。")
				continue
			}
			offset := 0
			filter := ""
			selected := false
			for !selected {
				matches := []personal.SessionInfo{}
				for _, s := range sessions {
					if filter == "" || strings.Contains(strings.ToLower(s.Title+" "+s.Workspace+" "+s.ID), strings.ToLower(filter)) {
						matches = append(matches, s)
					}
				}
				if offset >= len(matches) {
					offset = 0
				}
				end := min(len(matches), offset+10)
				fmt.Fprintln(status, "\n历史会话（最近更新优先）：")
				for i := offset; i < end; i++ {
					s := matches[i]
					fmt.Fprintf(status, "  %d  [%s] %s\n     %s · %s · %s\n", i-offset+1, s.Status, terminalText(s.Title), s.Updated.Local().Format("01-02 15:04"), terminalText(s.Workspace), s.ID)
				}
				fmt.Fprintln(status, "输入序号；n/p 翻页；/关键词 搜索；b 返回")
				pick, e := read("会话：")
				if e == io.EOF {
					return nil
				}
				if e != nil {
					return e
				}
				if pick == "b" {
					break
				}
				if pick == "n" {
					if end < len(matches) {
						offset = end
					}
					continue
				}
				if pick == "p" {
					offset = max(0, offset-10)
					continue
				}
				if strings.HasPrefix(pick, "/") {
					filter = strings.TrimPrefix(pick, "/")
					offset = 0
					continue
				}
				number, e := strconv.Atoi(pick)
				if e != nil || number < 1 || offset+number > end {
					fmt.Fprintln(status, "请输入列表中的序号。")
					continue
				}
				chosen := matches[offset+number-1]
				*session = chosen.ID
				if chosen.Launch != nil {
					p = *chosen.Launch
				} else {
					p.Workspace = chosen.Workspace
					fmt.Fprintln(status, "旧会话没有启动配置，将使用当前设置；可先调整以恢复原来的权限和资源。")
				}
				action := ""
				fmt.Fprintf(status, "\n选中：%s [%s]\n写文件：%s，固定诊断：%s，MCP：%s\n", terminalText(chosen.Title), chosen.Status, switchText(p.Write), switchText(p.Commands), optionalText(p.MCP))
				if chosen.Status == "未完成" {
					action = "-resume"
				}
				if chosen.Status == "需核对" {
					action = "-reconcile"
				}
				label := "继续聊天"
				if action == "-resume" {
					label = "恢复未完成任务"
				}
				if action == "-reconcile" {
					label = "核对操作结果（不重新执行）"
				}
				fmt.Fprintf(status, "Enter %s；s 调整启动设置；b 返回\n", label)
				confirm, e := read("操作：")
				if e == io.EOF {
					return nil
				}
				if e != nil {
					return e
				}
				if confirm == "b" {
					continue
				}
				if confirm == "s" {
					if e = editLaunch(&p, read, status); e != nil {
						return e
					}
				}
				if confirm != "" && confirm != "s" {
					continue
				}
				e = runOnce(ctx, launchArgs(p, *dir, *session, action), &lineInput{r}, out, status)
				if e != nil && !errors.Is(e, errMenu) {
					fmt.Fprintln(status, "启动/恢复失败：", e)
				}
				if action != "" && e == nil {
					fmt.Fprintln(status, "任务处理完毕，可再次选中此会话继续对话。")
				}
				selected = true
			}
			continue
		case "", "1":
			e = runOnce(ctx, launchArgs(p, *dir, "", ""), &lineInput{r}, out, status)
			if e != nil && !errors.Is(e, errMenu) {
				fmt.Fprintln(status, "会话结束：", e)
			}
		default:
			fmt.Fprintln(status, "请输入 0–3。")
		}
	}
}
func launchArgs(p personal.Launch, dir, id, action string) []string {
	args := []string{"-config", p.Config, "-workspace", p.Workspace, "-state-dir", dir, "-rounds", strconv.Itoa(p.Rounds), "-timeout", p.Timeout.String(), "-summary=" + strconv.FormatBool(p.Summary)}
	if p.Write {
		args = append(args, "-allow-write")
	}
	if p.Commands {
		args = append(args, "-allow-commands")
	}
	if p.MCP != "" {
		args = append(args, "-mcp-config", p.MCP)
	}
	if p.Exercise != "" {
		args = append(args, "-exercise", p.Exercise)
	}
	if id != "" {
		args = append(args, "-session", id)
	}
	if action != "" {
		args = append(args, action)
	}
	if p.Memory != nil {
		args = append(args, fmt.Sprintf("-memory=%t", *p.Memory))
	}
	if p.MemoryDB != "" {
		args = append(args, "-memory-db", p.MemoryDB)
	}
	if p.MemoryVectors {
		args = append(args, "-memory-vectors")
	}
	if p.MemoryCollection != "" {
		args = append(args, "-memory-collection", p.MemoryCollection)
	}
	return args
}
func editLaunch(p *personal.Launch, read func(string) (string, error), out io.Writer) error {
	for {
		fmt.Fprintf(out, "\n启动设置\n  1 工作目录：%s\n  2 资源配置：%s\n  3 写文件：%s\n  4 固定诊断：%s\n  5 MCP 配置：%s\n  6 代码题：%s\n  7 期限：%s\n  8 历史摘要：%s\n  0 返回\n", terminalText(p.Workspace), terminalText(p.Config), switchText(p.Write), switchText(p.Commands), optionalText(p.MCP), optionalText(p.Exercise), p.Timeout, switchText(p.Summary))
		c, e := read("设置项：")
		if e != nil {
			return e
		}
		if c == "0" || c == "" {
			return nil
		}
		switch c {
		case "3":
			p.Write = !p.Write
			continue
		case "4":
			p.Commands = !p.Commands
			continue
		case "8":
			p.Summary = !p.Summary
			continue
		}
		if c != "1" && c != "2" && c != "5" && c != "6" && c != "7" {
			continue
		}
		if c == "2" {
			fmt.Fprintln(out, "  1 轻量对话（无向量库）  2 知识检索与面试资源  3 自定义配置目录")
			preset, e := read("资源 [Enter 保留]：")
			if e != nil {
				return e
			}
			switch preset {
			case "":
				continue
			case "1":
				p.Config = "configs/harness"
				continue
			case "2":
				p.Config = "configs/interview"
				continue
			case "3":
			default:
				fmt.Fprintln(out, "请选择 1–3。")
				continue
			}
		}
		v, e := read("新值（Enter 保留，- 清空可选项）：")
		if e != nil {
			return e
		}
		if v == "" {
			continue
		}
		switch c {
		case "1":
			p.Workspace = v
		case "2":
			if v == "learning" {
				v = "configs/interview"
			}
			p.Config = v
		case "5":
			if v == "-" {
				v = ""
			}
			p.MCP = v
		case "6":
			if v == "-" {
				v = ""
			}
			p.Exercise = v
		case "7":
			d, e := time.ParseDuration(v)
			if e != nil || d <= 0 {
				fmt.Fprintln(out, "请输入正期限，例如 3m。")
				continue
			}
			p.Timeout = d
		}
	}
}
func absoluteLaunch(p personal.Launch) personal.Launch {
	if p.MemoryDB != "" {
		p.MemoryDB, _ = filepath.Abs(p.MemoryDB)
	}
	p.Config, _ = filepath.Abs(p.Config)
	p.Workspace, _ = filepath.Abs(p.Workspace)
	if p.MCP != "" {
		p.MCP, _ = filepath.Abs(p.MCP)
	}
	return p
}

func switchText(b bool) string {
	if b {
		return "开启"
	}
	return "关闭"
}
func optionalText(s string) string {
	if s == "" {
		return "未配置"
	}
	return terminalText(s)
}
