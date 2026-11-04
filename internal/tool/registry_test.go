package tool

import (
	"context"
	"strings"
	"testing"

	"github.com/lannokaede/LanAgent/internal/llm"
)

// 测试工具只用来检查注册表，不请求模型或读取知识库。
type registryTestTool struct{ name string }

func (t registryTestTool) Spec() llm.ToolDef {
	return llm.ToolDef{Name: t.name, Description: "注册表测试工具", Parameters: map[string]any{"type": "object"}}
}

func (t registryTestTool) Execute(context.Context, string) (string, error) {
	return t.name, nil
}

// 注册后应该能取到原工具，且工具仍然可以执行。
func TestRegistryRegisterLookupAndExecute(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(AddTool{}); err != nil {
		t.Fatal(err)
	}
	add, ok := r.Lookup("add")
	if !ok {
		t.Fatal("registered add tool was not found")
	}
	result, err := add.Execute(context.Background(), `{"a":2,"b":3}`)
	if err != nil || result != "5" {
		t.Fatalf("Execute: result=%q err=%v, want 5", result, err)
	}
	for _, name := range []string{"missing", "ADD", ""} {
		if _, ok := r.Lookup(name); ok {
			t.Fatalf("Lookup(%q) unexpectedly succeeded", name)
		}
	}
}

// 重复注册不能替换原工具，也不能向工具列表再插入一项。
func TestRegistryRejectsDuplicate(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(AddTool{}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(registryTestTool{name: "add"}); err == nil {
		t.Fatal("duplicate tool name should be rejected")
	}
	add, ok := r.Lookup("add")
	if !ok {
		t.Fatal("duplicate registration removed original tool")
	}
	result, err := add.Execute(context.Background(), `{"a":2,"b":3}`)
	if err != nil || result != "5" {
		t.Fatalf("duplicate registration replaced original tool: %q %v", result, err)
	}
	if len(r.ListSpecs()) != 1 || len(r.ListTools()) != 1 {
		t.Fatal("duplicate registration changed list length")
	}
}

// 名称规则：小写字母开头，总长度不超过 64，只允许小写字母、数字、下划线。
func TestRegistryNameValidation(t *testing.T) {
	for _, name := range []string{"", "ADD", " add", "add ", "1add", "_private", "knowledge-search", "知识检索", strings.Repeat("a", 65)} {
		t.Run("invalid/"+name, func(t *testing.T) {
			r := NewRegistry()
			if err := r.Register(registryTestTool{name: name}); err == nil {
				t.Fatalf("invalid name %q should be rejected", name)
			}
			if len(r.ListSpecs()) != 0 || len(r.ListTools()) != 0 {
				t.Fatal("rejected registration changed registry")
			}
		})
	}
	for _, name := range []string{"a", "add", "knowledge_search", "tool_2", strings.Repeat("a", 64)} {
		t.Run("valid/"+name, func(t *testing.T) {
			r := NewRegistry()
			if err := r.Register(registryTestTool{name: name}); err != nil {
				t.Fatalf("valid name %q rejected: %v", name, err)
			}
		})
	}
}

// 空注册表可以安全读取，nil 工具必须被拒绝。
func TestRegistryEmptyAndNil(t *testing.T) {
	r := NewRegistry()
	if len(r.ListSpecs()) != 0 || len(r.ListTools()) != 0 {
		t.Fatal("new registry should be empty")
	}
	if _, ok := r.Lookup("add"); ok {
		t.Fatal("empty registry should not contain add")
	}
	if err := r.Register(nil); err == nil {
		t.Fatal("nil tool should be rejected")
	}
}

// 列表必须遵循注册顺序，且调用方修改列表不能破坏注册表。
func TestRegistryListsPreserveOrderAndOwnership(t *testing.T) {
	r := NewRegistry()
	first := &registryTestTool{name: "z_tool"}
	second := &registryTestTool{name: "a_tool"}
	for _, item := range []Tool{first, second} {
		if err := r.Register(item); err != nil {
			t.Fatal(err)
		}
	}
	// 重复读取也要保持顺序。
	for i := 0; i < 20; i++ {
		specs := r.ListSpecs()
		if len(specs) != 2 || specs[0].Name != "z_tool" || specs[1].Name != "a_tool" {
			t.Fatalf("unexpected spec order: %+v", specs)
		}
		if specs[0].Description != first.Spec().Description || specs[0].Parameters["type"] != "object" {
			t.Fatal("tool description or schema was lost")
		}
		registered := r.ListTools()
		if len(registered) != 2 || registered[0] != first || registered[1] != second {
			t.Fatal("ListTools lost registration order or original instances")
		}
	}
	specs := r.ListSpecs()
	specs[0].Name = "changed"
	registered := r.ListTools()
	registered[0] = nil
	if r.ListSpecs()[0].Name != "z_tool" || r.ListTools()[0] != first {
		t.Fatal("caller mutation changed registry")
	}
}
