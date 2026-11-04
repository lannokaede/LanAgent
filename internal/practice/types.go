// Package practice implements versioned single-file Go exercises, independently of chat memory.
package practice

import (
	"context"
	"errors"
	"github.com/lannokaede/LanAgent/internal/llm"
	"time"
)

var (
	ErrInput       = errors.New("输入无效：代码最多32 KiB，问题最多2000字，须选择有效练习版本和请求ID")
	ErrNotFound    = errors.New("记录不存在")
	ErrConflict    = errors.New("请求ID已用于不同内容")
	ErrUnavailable = errors.New("隔离测试服务不可用，请检查Docker和练习镜像")
	ErrBusy        = errors.New("任务队列已满，请稍后重试")
)

const MaxCodeBytes = 32 << 10

type Event struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

type Exercise struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Signature   string `json:"signature"`
	StarterCode string `json:"starter_code"`
	Tests       string `json:"-"`
}
type Submit struct {
	RequestID  string `json:"request_id"`
	ExerciseID string `json:"exercise_id"`
	Version    string `json:"version"`
	Code       string `json:"code"`
	Question   string `json:"question"`
}
type Attempt struct {
	ID string `json:"id"`
	Submit
	CreatedAt time.Time `json:"created_at"`
	Reviews   []Review  `json:"reviews"`
	Runs      []Run     `json:"runs"`
}
type Finding struct {
	Line    int    `json:"line"`
	EndLine int    `json:"end_line"`
	Message string `json:"message"`
}
type Evidence struct {
	Source     string `json:"source"`
	ChunkIndex int    `json:"chunk_index"`
	Quote      string `json:"quote"`
}
type CandidateVerification struct {
	CodeHash string `json:"code_hash"`
	Status   string `json:"status"`
}
type Diagnosis struct {
	Verification *CandidateVerification `json:"verification,omitempty"`
	Summary      string                 `json:"summary"`
	Findings     []Finding              `json:"findings"`
	Evidence     []Evidence             `json:"evidence"`
	ProposedCode string                 `json:"proposed_code"`
	Limitations  []string               `json:"limitations"`
}
type Review struct {
	ID        string     `json:"id"`
	AttemptID string     `json:"attempt_id"`
	RequestID string     `json:"request_id"`
	Status    string     `json:"status"`
	Result    *Diagnosis `json:"result,omitempty"`
	Trace     []Event    `json:"trace"`
	Model     string     `json:"model"`
	Usage     llm.Usage  `json:"usage"`
	Error     string     `json:"error,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}
type RunRequest struct {
	RequestID string `json:"request_id"`
	ReviewID  string `json:"review_id,omitempty"`
}
type Run struct {
	ID        string `json:"id"`
	AttemptID string `json:"attempt_id"`
	RunRequest
	CodeHash   string    `json:"code_hash"`
	Status     string    `json:"status"`
	ExitCode   int       `json:"exit_code"`
	DurationMS int64     `json:"duration_ms"`
	Output     string    `json:"output"`
	Truncated  bool      `json:"truncated"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}
type Executor interface {
	Health(context.Context) error
	Cleanup(context.Context) error
	Execute(context.Context, Exercise, string, string) Run
}
