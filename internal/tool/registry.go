package tool

import (
	"fmt"
	"regexp"

	"github.com/lannokaede/LanAgent/internal/llm"
)

// 工具名称统一使用小写字母、数字和下划线。
// 必须以小写字母开头，例如 add、knowledge_search。
// 统一名称格式，避免框架忽略大小写时发生名称冲突。
var validToolName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Registry 保存已经注册的工具。
//
// 使用方式：
//  1. 程序启动时创建 Registry。
//  2. 启动时注册全部工具。
//  3. 请求开始后，只查找和读取，不再修改注册表。
//
// 当前实现不支持运行期间动态注册工具。
type Registry struct {
	// 用工具名查找工具。
	byName map[string]Tool

	// 保存注册顺序，使发给模型的工具列表顺序稳定。
	order []string
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{
		byName: make(map[string]Tool),
	}
}

// Register 注册一个工具。
// 同一个名称只能注册一次。
func (r *Registry) Register(t Tool) error {
	if t == nil {
		return fmt.Errorf("tool cannot be nil")
	}

	spec := t.Spec()

	if !validToolName.MatchString(spec.Name) {
		return fmt.Errorf("invalid tool name %q", spec.Name)
	}

	if _, exists := r.byName[spec.Name]; exists {
		return fmt.Errorf("tool %q already registered", spec.Name)
	}

	r.byName[spec.Name] = t
	r.order = append(r.order, spec.Name)

	return nil
}

// Lookup 根据工具名称查找实际工具。
// 第二个返回值表示是否找到。
func (r *Registry) Lookup(name string) (Tool, bool) {
	t, exists := r.byName[name]
	return t, exists
}

// ListSpecs 返回全部工具的描述，供模型选择工具。
func (r *Registry) ListSpecs() []llm.ToolDef {
	specs := make([]llm.ToolDef, 0, len(r.order))

	for _, name := range r.order {
		t := r.byName[name]
		specs = append(specs, t.Spec())
	}

	return specs
}

// ListTools 返回全部实际工具，供调用者读取实际工具。
func (r *Registry) ListTools() []Tool {
	registered := make([]Tool, 0, len(r.order))

	for _, name := range r.order {
		registered = append(registered, r.byName[name])
	}

	return registered
}
