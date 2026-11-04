// Command setup imports or interactively configures shared local model APIs.
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
	"strconv"
	"strings"
	"time"

	"github.com/lannokaede/LanAgent/internal/apiconfig"
	"github.com/lannokaede/LanAgent/internal/config"
	"golang.org/x/term"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() { <-ctx.Done(); _ = os.Stdin.Close() }()
	if e := run(ctx, os.Args[1:], os.Stdin, os.Stdout); e != nil {
		fmt.Fprintln(os.Stderr, "配置失败：", e)
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	f := flag.NewFlagSet("setup", flag.ContinueOnError)
	f.SetOutput(out)
	kind := f.String("kind", "chat", "chat 或 embedding")
	importFile := f.String("import", "", "粘贴块/JSON/YAML/.env 文件；- 表示从标准输入粘贴，/save 结束")
	fromConfig := f.String("from-config", "", "从已有私有配置目录导入")
	model := f.String("model", "", "导入时指定或覆盖模型名")
	path := f.String("file", "", "共享 API 配置文件，默认用户配置目录")
	show := f.Bool("show", false, "查看已保存配置，密钥仅显示是否配置")
	checkOnly := f.Bool("check", false, "检查已保存 API；聊天发送一次极小请求")
	skipCheck := f.Bool("skip-check", false, "只保存，不发送连通性请求")
	if e := f.Parse(args); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return nil
		}
		return e
	}
	if f.NArg() != 0 || (*kind != "chat" && *kind != "embedding") || (*importFile != "" && *fromConfig != "") || (*show && *checkOnly) || ((*show || *checkOnly) && (*importFile != "" || *fromConfig != "")) {
		return errors.New("请选择 chat/embedding，导入方式及查看/检查操作不能混用")
	}
	if *path == "" {
		p, e := config.APISettingsPath()
		if e != nil {
			return e
		}
		*path = p
	}
	settings, e := config.ReadAPISettings(*path)
	if e != nil {
		return e
	}
	endpoint := settings.Chat
	if *kind == "embedding" {
		endpoint = settings.Embedding
	}
	if *show {
		fmt.Fprintln(out, "本机共享配置：", *path)
		display(out, "Chat", settings.Chat)
		display(out, "Embedding", settings.Embedding)
		fmt.Fprintln(out, "运行时优先级：环境变量 > 项目 config.local.yaml > 本机共享 API > 项目基础配置。")
		return nil
	}
	if *checkOnly {
		if e = validate(&endpoint, *kind); e != nil {
			return e
		}
		return check(ctx, out, endpoint, *kind)
	}
	reader := bufio.NewReader(in)
	switch {
	case *fromConfig != "":
		loaded, e := config.Load(*fromConfig)
		if e != nil {
			return e
		}
		if *kind == "embedding" {
			endpoint = config.APIEndpoint{BaseURL: loaded.Embedding.BaseURL, Model: loaded.Embedding.Model, APIKey: loaded.Embedding.APIKey, Dimensions: loaded.Embedding.Dimensions}
		} else {
			endpoint = config.APIEndpoint{BaseURL: loaded.LLM.BaseURL, Model: loaded.LLM.Model, APIKey: loaded.LLM.APIKey}
		}
	case *importFile != "":
		var text []byte
		if *importFile == "-" {
			fmt.Fprintln(out, "粘贴配置，最后单独输入 /save；也可 EOF 结束。终端粘贴输入将隐藏。")
			var body strings.Builder
			for {
				var line string
				var e error
				if file, ok := in.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
					var data []byte
					data, e = term.ReadPassword(int(file.Fd()))
					line = string(data) + "\n"
					fmt.Fprintln(out)
				} else {
					line, e = reader.ReadString('\n')
				}
				if strings.TrimSpace(line) == "/save" {
					break
				}
				body.WriteString(line)
				if body.Len() > 64<<10 {
					return errors.New("导入内容超过 64 KiB")
				}
				if e == io.EOF {
					break
				}
				if e != nil {
					return e
				}
			}
			text = []byte(body.String())
		} else {
			file, e := os.Open(*importFile)
			if e != nil {
				return errors.New("无法读取 API 导入文件")
			}
			text, e = io.ReadAll(io.LimitReader(file, 64<<10+1))
			file.Close()
			if e != nil {
				return errors.New("无法读取 API 导入文件")
			}
		}
		endpoint, e = apiconfig.ParseImport(string(text), *kind)
		if e != nil {
			return e
		}
	default:
		fmt.Fprintln(out, "配置模型 API：填写 Base URL、Key、模型名；回车保留已有值。")
		fmt.Fprintln(out, "保存前默认发送一次最小连通性请求，-skip-check 可仅保存。")
		oldURL := endpoint.BaseURL
		endpoint.BaseURL, e = ask(reader, out, "Base URL", endpoint.BaseURL)
		if e != nil {
			return e
		}
		normalized, e := config.NormalizeAPIURL(endpoint.BaseURL)
		if e != nil {
			return e
		}
		endpoint.BaseURL = normalized
		if oldURL != "" && strings.TrimRight(oldURL, "/") != normalized {
			endpoint.APIKey = ""
			endpoint.Model = ""
		}
		endpoint.APIKey, e = secret(reader, in, out, endpoint.APIKey)
		if e != nil {
			return e
		}
		prompt := "模型名（留空尝试自动获取）"
		if *kind == "embedding" {
			prompt = "Embedding 模型名"
			if endpoint.Model == "" {
				endpoint.Model = "text-embedding-v4"
			}
		}
		endpoint.Model, e = ask(reader, out, prompt, endpoint.Model)
		if e != nil {
			return e
		}
		if *kind == "embedding" {
			current := endpoint.Dimensions
			if current == 0 {
				current = 1024
			}
			value, e := ask(reader, out, "向量维度", strconv.Itoa(current))
			if e != nil {
				return e
			}
			endpoint.Dimensions, e = strconv.Atoi(value)
			if e != nil {
				return errors.New("向量维度必须为整数")
			}
		}
	}
	if *model != "" {
		endpoint.Model = *model
	}
	endpoint.BaseURL, e = config.NormalizeAPIURL(endpoint.BaseURL)
	if e != nil {
		return e
	}
	if endpoint.APIKey == "" {
		return errors.New("缺少 API Key")
	}
	if endpoint.Model == "" && *kind == "chat" {
		models, e := apiconfig.Models(ctx, endpoint)
		if e != nil {
			fmt.Fprintln(out, "无法自动获取模型，请填写服务商提供的模型名。否则可用 -model 指定。")
			endpoint.Model, e = ask(reader, out, "模型名", "")
			if e != nil {
				return e
			}
		} else if len(models) == 1 {
			endpoint.Model = models[0]
			fmt.Fprintln(out, "自动选择模型：", endpoint.Model)
		} else {
			for i, name := range models[:min(len(models), 30)] {
				fmt.Fprintf(out, "%d. %s\n", i+1, name)
			}
			value, e := ask(reader, out, "选择序号或输入完整模型名", "1")
			if e != nil {
				return e
			}
			if n, e := strconv.Atoi(value); e == nil {
				if n < 1 || n > min(len(models), 30) {
					return errors.New("模型序号无效")
				}
				endpoint.Model = models[n-1]
			} else {
				endpoint.Model = value
			}
		}
	}
	if e = validate(&endpoint, *kind); e != nil {
		return e
	}
	if !*skipCheck {
		if e = check(ctx, out, endpoint, *kind); e != nil {
			return fmt.Errorf("%w；尚未覆盖已有配置，可修正后重试或使用 -skip-check", e)
		}
	}
	if *kind == "embedding" {
		settings.Embedding = endpoint
	} else {
		settings.Chat = endpoint
	}
	if e = config.WriteAPISettings(*path, settings); e != nil {
		return e
	}
	fmt.Fprintln(out, "已保存本机共享 API 配置：", *path)
	fmt.Fprintln(out, "harness、interview、knowledge 会自动读取，无需分别修改配置。")
	fmt.Fprintln(out, "已有环境变量或项目 config.local.yaml 的同名字段仍优先。")
	if *path != mustDefaultPath() {
		fmt.Fprintln(out, "使用该自定义文件时请设置 LANAGENT_API_CONFIG，或改用默认配置路径。")
	}
	return nil
}
func mustDefaultPath() string { p, _ := config.APISettingsPath(); return p }
func validate(e *config.APIEndpoint, kind string) error {
	url, err := config.NormalizeAPIURL(e.BaseURL)
	if err != nil {
		return err
	}
	e.BaseURL = url
	e.Model = strings.TrimSpace(e.Model)
	e.APIKey = strings.TrimSpace(e.APIKey)
	if e.APIKey == "" || e.Model == "" || len(e.Model) > 200 || strings.ContainsAny(e.APIKey+e.Model, "\r\n") {
		return errors.New("请提供有效 API Key 和模型名")
	}
	if kind == "embedding" {
		if e.Model != "text-embedding-v4" {
			return errors.New("当前知识库适配器使用 text-embedding-v4；其他模型需先扩展适配器并使用新集合")
		}
		if e.Dimensions == 0 {
			e.Dimensions = 1024
		}
		if e.Dimensions < 1 {
			return errors.New("向量维度必须大于 0")
		}
	}
	return nil
}
func ask(r *bufio.Reader, out io.Writer, label, current string) (string, error) {
	if current != "" {
		fmt.Fprintf(out, "%s [%s]：", label, current)
	} else {
		fmt.Fprint(out, label, "：")
	}
	line, e := r.ReadString('\n')
	if e != nil && e != io.EOF {
		return "", e
	}
	value := strings.TrimSpace(line)
	if value == "" {
		return current, nil
	}
	return value, nil
}
func secret(r *bufio.Reader, in io.Reader, out io.Writer, current string) (string, error) {
	hint := ""
	if current != "" {
		hint = "（已配置，回车保留）"
	}
	fmt.Fprint(out, "API Key", hint, "：")
	if file, ok := in.(*os.File); ok && term.IsTerminal(int(file.Fd())) && r.Buffered() == 0 {
		data, e := term.ReadPassword(int(file.Fd()))
		fmt.Fprintln(out)
		if e != nil {
			return "", errors.New("无法读取密钥")
		}
		if len(data) == 0 {
			return current, nil
		}
		return strings.TrimSpace(string(data)), nil
	}
	line, e := r.ReadString('\n')
	if e != nil && e != io.EOF {
		return "", e
	}
	value := strings.TrimSpace(line)
	if value == "" {
		return current, nil
	}
	return value, nil
}
func display(out io.Writer, label string, e config.APIEndpoint) {
	configured := "未配置"
	if e.APIKey != "" {
		configured = "已配置（隐藏）"
	}
	fmt.Fprintf(out, "%s：URL=%s，模型=%s，Key=%s", label, e.BaseURL, e.Model, configured)
	if e.Dimensions > 0 {
		fmt.Fprintf(out, "，维度=%d", e.Dimensions)
	}
	fmt.Fprintln(out)
}
func check(ctx context.Context, out io.Writer, e config.APIEndpoint, kind string) error {
	call, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	start := time.Now()
	if err := apiconfig.Check(call, e, kind); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s API 连通成功，模型=%s，耗时=%s\n", kind, e.Model, time.Since(start).Round(time.Millisecond))
	return nil
}
