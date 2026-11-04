// Command interview is a convenience alias for the unified conversation CLI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/lannokaede/LanAgent/internal/cli"
	"github.com/lannokaede/LanAgent/internal/interview"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); _ = os.Stdin.Close() }()
	if e := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); e != nil {
		fmt.Fprintln(os.Stderr, "Error:", e)
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string, in io.Reader, out, status io.Writer) error {
	f := flag.NewFlagSet("interview", flag.ContinueOnError)
	f.SetOutput(status)
	config := f.String("config", "configs/interview", "shared model and knowledge profile")
	topic := f.String("topic", "all", "initial interview topic")
	questions := f.Int("questions", 5, "requested question count (1..16)")
	workspace := f.String("workspace", ".", "shared workspace")
	state := f.String("state-dir", "", "shared conversation state directory")
	session := f.String("session", "", "unified conversation id; original standalone JSON is retained separately")
	if e := f.Parse(args); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return nil
		}
		return e
	}
	if f.NArg() != 0 || len(interview.Questions(*topic)) == 0 || *questions < 1 || *questions > 16 {
		return errors.New("invalid question count or positional arguments")
	}
	if strings.ContainsAny(*session, "/\\.") {
		return errors.New("-session now accepts the unified conversation ID; old standalone JSON files remain unchanged")
	}
	call := []string{"-config", *config, "-workspace", *workspace}
	if *state != "" {
		call = append(call, "-state-dir", *state)
	}
	if *session != "" {
		call = append(call, "-session", *session)
	}
	initial := fmt.Sprintf("请开始或继续 Go 后端模拟面试，主题 %s，本轮 %d 题，一问一答。\n", *topic, *questions)
	fmt.Fprintln(status, "interview 已作为统一 Harness 的兼容启动入口，直接输入答案或其他任务即可。")
	return cli.Run(ctx, call, io.MultiReader(strings.NewReader(initial), in), out, status)
}
