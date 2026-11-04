// Package tool 保存实际工具能力，不负责模型推理、HTTP 或会话管理。
// 模块 1：Tool 的 Spec 提供名称/说明/JSON Schema；Execute 执行模型提出的调用。
// 模块 2：AddTool 提供加法 Schema，严格检查 JSON 大小、必需字段、类型和有限数值。
// 参数始终是不可信输入；模型声明调用不能代替程序校验。
// 模块 3：harness/tools.go 直接执行领域 Tool，
// 由 LangGraphGo 状态图调度；普通工具失败作为观察送回模型，取消则立即终止。
// 模块 4：Registry 在启动时注册工具，图节点按名称精确查找并执行。
// Harness ToolPolicy 可配置统一的工具超时、调用次数和结果上限。
package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/lannokaede/LanAgent/internal/llm"
)

type Tool interface {
	Spec() llm.ToolDef
	Execute(ctx context.Context, argsJSON string) (string, error)
}

type AddTool struct{}

func (AddTool) Spec() llm.ToolDef {
	return llm.ToolDef{
		Name:        "add",
		Description: "计算两个数字的和",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"a": map[string]any{"type": "number"},
				"b": map[string]any{"type": "number"},
			},
			"required":             []string{"a", "b"},
			"additionalProperties": false,
		},
	}
}

func (AddTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(argsJSON) > 1024 {
		return "", fmt.Errorf("tool arguments are too large")
	}

	// 指针用于区分“参数没提供”和“参数是 0”。
	var args struct {
		A *float64 `json:"a"`
		B *float64 `json:"b"`
	}

	decoder := json.NewDecoder(strings.NewReader(argsJSON))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&args); err != nil {
		return "", fmt.Errorf("decode add arguments: %w", err)
	}
	if args.A == nil || args.B == nil {
		return "", fmt.Errorf("add requires both a and b")
	}

	// 确认参数后没有第二段 JSON。
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return "", fmt.Errorf("add arguments must contain one JSON object")
	}

	sum := *args.A + *args.B
	if math.IsInf(sum, 0) {
		return "", fmt.Errorf("add result is out of range")
	}
	return strconv.FormatFloat(sum, 'f', -1, 64), nil
}

// ErrUncertain means an external effect may have happened without a reliable
// acknowledgement. A durable harness must reconcile it instead of retrying.
var ErrUncertain = errors.New("tool outcome requires reconciliation")
