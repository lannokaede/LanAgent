package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/lannokaede/LanAgent/internal/config"
	"github.com/lannokaede/LanAgent/internal/filelock"
	"github.com/lannokaede/LanAgent/internal/interview"
	"github.com/lannokaede/LanAgent/internal/llm"
	"github.com/lannokaede/LanAgent/internal/rag"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() { <-ctx.Done(); _ = os.Stdin.Close() }()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	f := flag.NewFlagSet("interview", flag.ContinueOnError)
	f.SetOutput(out)
	configDir := f.String("config", "configs/interview", "configuration directory")
	topic := f.String("topic", "all", "all/go/concurrency/runtime/database/redis/engineering")
	count := f.Int("questions", 5, "maximum questions per session (1..16)")
	logDir := f.String("log-dir", ".runtime/interviews", "transcript directory")
	sessionPath := f.String("session", "", "继续已有面试状态文件；省略则创建新会话")
	followups := f.Bool("followups", true, "根据回答遗漏追问，每个基础题最多一次")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 || *count < 1 || *count > 16 || len(interview.Questions(*topic)) == 0 {
		return fmt.Errorf("invalid topic or question count")
	}
	cfg, err := config.Load(*configDir)
	if err != nil {
		return err
	}
	engine, store, err := rag.FromConfig(cfg)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Check(ctx); err != nil {
		return err
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
	coach, err := interview.New(engine, gateway, cfg.RAG.KnowledgeBase, *topic)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*logDir, 0700); err != nil {
		return err
	}
	coach.WithFollowups(*followups)
	if *sessionPath == "" {
		*sessionPath = filepath.Join(*logDir, time.Now().UTC().Format("20060102T150405Z")+"-"+rand.Text()+".json")
	}
	if e := os.MkdirAll(filepath.Dir(*sessionPath), 0700); e != nil {
		return e
	}
	guard, e := os.OpenFile(*sessionPath+".lock", os.O_RDWR|os.O_CREATE, 0600)
	if e != nil {
		return e
	}
	defer guard.Close()
	if e = filelock.Lock(guard); e != nil {
		return fmt.Errorf("interview session is already open: %w", e)
	}
	defer filelock.Unlock(guard)
	if _, e := os.Stat(*sessionPath); e == nil {
		if e = coach.Load(*sessionPath); e != nil {
			return e
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	path := strings.TrimSuffix(*sessionPath, ".json") + ".jsonl"
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	log, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	fmt.Fprintf(out, "会话状态：%s\n复盘记录：%s\n", *sessionPath, strings.TrimSuffix(*sessionPath, ".json")+"-review.md")
	fmt.Fprintf(out, "Go 后端模拟面试 · %s\n一题一答；答案末尾输入单独一行 /submit 提交，/exit 退出。\n记录：%s\n", *topic, path)
	reader := bufio.NewScanner(in)
	reader.Buffer(make([]byte, 4096), 32<<10)
	for n := 0; n < *count; n++ {
		call, cancel := context.WithTimeout(ctx, 60*time.Second)
		q, err := coach.Next(call)
		cancel()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if err := coach.Save(*sessionPath); err != nil {
			return err
		}
		fmt.Fprintf(out, "\n第 %d 题 [%s]：%s\n你的回答：\n", n+1, q.Topic, q.Text)
		var lines []string
		for reader.Scan() {
			line := reader.Text()
			if strings.TrimSpace(line) == "/exit" {
				return nil
			}
			if strings.TrimSpace(line) == "/submit" {
				break
			}
			lines = append(lines, line)
			if len(strings.Join(lines, "\n")) > 16000 {
				return fmt.Errorf("answer too long")
			}
		}
		if err := reader.Err(); err != nil {
			return err
		}
		answer := strings.TrimSpace(strings.Join(lines, "\n"))
		if answer == "" {
			return nil
		}
		call, cancel = context.WithTimeout(ctx, 90*time.Second)
		round, err := coach.Answer(call, answer)
		cancel()
		if err != nil {
			return err
		}
		if err := coach.Save(*sessionPath); err != nil {
			return err
		}
		if err := os.WriteFile(strings.TrimSuffix(*sessionPath, ".json")+"-review.md", []byte(coach.ReviewMarkdown()), 0600); err != nil {
			return err
		}
		if err := json.NewEncoder(log).Encode(round); err != nil {
			return err
		}
		if err := log.Sync(); err != nil {
			return err
		}
		fmt.Fprintf(out, "\n参考评分：%d/10\n点评：%s\n参考回答：%s\n依据：\n", round.Evaluation.Score, round.Evaluation.Feedback, round.Evaluation.ReferenceAnswer)
		if d := round.Evaluation.Dimensions; len(d) > 0 {
			fmt.Fprintf(out, "知识点：%d/10 · 完整性：%d/10 · 表达：%d/10\n", d["knowledge"], d["completeness"], d["expression"])
		}
		seen := map[int]bool{}
		for _, ref := range round.Evaluation.References {
			if seen[ref] {
				continue
			}
			seen[ref] = true
			h := round.Evidence[ref-1]
			fmt.Fprintf(out, "[%d] %s · chunk %d\n", ref, h.Source, h.Index)
		}
	}
	fmt.Fprintf(out, "\n本轮完成，共 %d 题。\n", len(coach.History))
	return nil
}
