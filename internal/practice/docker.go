package practice

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const containerLabel = "lanagent.practice.owner"
const MaxOutputBytes = 64 << 10

type boundedOutput struct {
	mu        sync.Mutex
	data      []byte
	truncated bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	remaining := MaxOutputBytes - len(b.data)
	if len(p) > remaining {
		b.truncated = true
		p = p[:remaining]
	}
	b.data = append(b.data, p...)
	return n, nil
}
func (b *boundedOutput) Text() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	value := strings.ToValidUTF8(string(b.data), "�")
	if len(value) > MaxOutputBytes {
		value = value[:MaxOutputBytes]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
		return value, true
	}
	return value, b.truncated
}

type DockerExecutor struct {
	Binary, Image, Owner, WorkDir string
	mu                            sync.Mutex
	cleaned                       bool
}

func NewDockerExecutor(image, user, workDir string) *DockerExecutor {
	return &DockerExecutor{Binary: "docker", Image: image, Owner: digest(user)[:20], WorkDir: workDir}
}
func (d *DockerExecutor) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, d.Binary, args...)
	cmd.WaitDelay = 2 * time.Second
	return cmd
}
func (d *DockerExecutor) Health(ctx context.Context) error {
	if d.Image == "" {
		return ErrUnavailable
	}
	if err := d.command(ctx, "version", "--format", "{{.Server.Version}}").Run(); err != nil {
		return ErrUnavailable
	}
	if err := d.command(ctx, "image", "inspect", d.Image).Run(); err != nil {
		return ErrUnavailable
	}
	d.mu.Lock()
	cleaned := d.cleaned
	d.mu.Unlock()
	if !cleaned {
		return d.Cleanup(ctx)
	}
	return nil
}
func (d *DockerExecutor) Cleanup(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	output, err := d.command(ctx, "ps", "-aq", "--filter", "label="+containerLabel+"="+d.Owner).Output()
	if err != nil {
		return ErrUnavailable
	}
	ids := strings.Fields(string(output))
	for _, id := range ids {
		if !regexp.MustCompile(`^[a-f0-9]{12,64}$`).MatchString(id) {
			return ErrUnavailable
		}
		if err = d.command(ctx, "rm", "-f", id).Run(); err != nil {
			return ErrUnavailable
		}
	}
	d.cleaned = true
	return nil
}
func (d *DockerExecutor) Execute(parent context.Context, e Exercise, code, taskID string) (r Run) {
	r = Run{Status: "infrastructure_error", ExitCode: -1}
	start := time.Now()
	defer func() { r.DurationMS = time.Since(start).Milliseconds() }()
	if err := ExecutionPolicy(code); err != nil {
		r.Error = "代码超出练习执行范围"
		return
	}
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	if err := d.Health(ctx); err != nil {
		r.Error = ErrUnavailable.Error()
		return
	}
	root, err := filepath.Abs(d.WorkDir)
	if err != nil {
		r.Error = "无法创建临时工作区"
		return
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		r.Error = "无法创建临时工作区"
		return
	}
	dir, err := os.MkdirTemp(root, "run-")
	if err != nil {
		r.Error = "无法创建临时工作区"
		return
	}
	defer os.RemoveAll(dir)
	if strings.Contains(dir, ",") {
		r.Error = "运行器路径不能包含逗号"
		return
	}
	// Container sees only these three files. Tests and module never come from client/model.
	for name, contents := range map[string]string{"solution.go": code, "exercise_test.go": e.Tests, "go.mod": "module exercise\n\ngo 1.26.0\n"} {
		if err = os.WriteFile(filepath.Join(dir, name), []byte(contents), 0444); err != nil {
			r.Error = "无法写入测试快照"
			return
		}
	}
	if err = os.Chmod(dir, 0755); err != nil {
		r.Error = "无法准备测试快照"
		return
	}
	name := "lanagent-practice-" + d.Owner + "-" + digest(taskID)[:20]
	defer func() {
		cleanup, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		if err := d.command(cleanup, "rm", "-f", name).Run(); err != nil {
			d.mu.Lock()
			d.cleaned = false
			d.mu.Unlock()
		}
	}()
	args := []string{"create", "--name", name, "--label", containerLabel + "=" + d.Owner, "--network", "none", "--user", "10001:10001", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--memory", "1g", "--cpus", "1", "--pids-limit", "128", "--log-driver", "none", "--tmpfs", "/tmp:rw,exec,nosuid,size=768m,mode=1777", "--mount", "type=bind,src=" + dir + ",dst=/workspace,readonly", "--workdir", "/workspace", "--env", "GOTOOLCHAIN=local", "--env", "GOPROXY=off", "--env", "GOSUMDB=off", "--env", "GOMAXPROCS=2", d.Image, "go", "test", "-json", "-race", "-count=1", "-timeout=10s", "./..."}
	if err = d.command(ctx, args...).Run(); err != nil {
		r.Error = "无法创建隔离测试容器"
		return
	}
	output := &boundedOutput{}
	cmd := d.command(ctx, "start", "-a", name)
	cmd.Stdout = output
	cmd.Stderr = output
	runErr := cmd.Run()
	r.Output, r.Truncated = output.Text()
	if ctx.Err() != nil {
		r.Status = "timeout"
		r.Error = "测试超时，容器已强制清理"
		if parent.Err() != nil {
			r.Status = "interrupted"
			r.Error = "服务停止中断测试"
		}
		return
	}
	inspect, err := d.command(ctx, "inspect", "--format", "{{json .State}}", name).Output()
	if err != nil {
		r.Error = "无法读取容器退出状态"
		return
	}
	var state struct {
		Running   bool
		ExitCode  int
		OOMKilled bool
		Error     string
	}
	if json.Unmarshal(inspect, &state) != nil || state.Running {
		r.Error = "容器状态无效"
		return
	}
	r.ExitCode = state.ExitCode
	if state.OOMKilled || state.Error != "" {
		r.Error = "容器资源不足或运行环境异常"
		return
	}
	// Exit code alone is insufficient: require a complete package test terminal event.
	r.Status = ClassifyTestOutput(r.Output, r.ExitCode, r.Truncated)
	if runErr != nil && r.Status == "passed" {
		r.Status = "infrastructure_error"
	}
	if r.Status == "infrastructure_error" {
		r.Error = "测试运行未产生完整可校验的结果"
	}
	return
}
func ClassifyTestOutput(output string, exit int, truncated bool) string {
	if strings.Contains(output, "fork/exec ") && strings.Contains(output, "permission denied") {
		return "infrastructure_error"
	}
	if strings.Contains(output, "WARNING: DATA RACE") {
		return "race_failed"
	}
	if strings.Contains(output, "test timed out after") {
		return "timeout"
	}
	if strings.Contains(output, "[build failed]") || strings.Contains(output, "build-fail") || strings.Contains(output, "undefined:") || strings.Contains(output, "syntax error:") {
		return "compile_failed"
	}
	var terminal string
	for _, line := range strings.Split(output, "\n") {
		var ev struct{ Action, Package, Test string }
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Package == "exercise" && ev.Test == "" && (ev.Action == "pass" || ev.Action == "fail") {
			terminal = ev.Action
		}
	}
	if exit == 0 && terminal == "pass" && !truncated {
		return "passed"
	}
	if exit != 0 && terminal == "fail" {
		return "failed"
	}
	return "infrastructure_error"
}

var _ io.Writer = (*boundedOutput)(nil)
var _ Executor = (*DockerExecutor)(nil)
