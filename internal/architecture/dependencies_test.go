package architecture

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const repositoryModule = "github.com/lannokaede/LanAgent"

type dependencyRule struct {
	packagePath string
	allowed     []string
}

// These three packages provide the reusable execution core. Configuration,
// persistence and terminal integration belong to their callers, not the core.
var corePackageRules = []dependencyRule{
	{"internal/harness", []string{"internal/contextbudget", "internal/llm", "internal/tool", "internal/observability/telemetry"}},
	{"internal/llm", []string{"internal/engineering", "internal/observability/telemetry"}},
	{"internal/contextbudget", []string{"internal/llm"}},
}

func TestCorePackageDependencies(t *testing.T) {
	for _, rule := range corePackageRules {
		t.Run(rule.packagePath, func(t *testing.T) {
			violations, err := inspectDependencies(filepath.Join("..", ".."), rule)
			if err != nil {
				t.Fatal(err)
			}
			for _, violation := range violations {
				t.Errorf("%s:%d: %s must not import %s; allowed repository dependencies: %s",
					violation.file, violation.line, rule.packagePath, violation.dependency, strings.Join(rule.allowed, ", "))
			}
		})
	}
}

type dependencyViolation struct {
	file       string
	line       int
	dependency string
}

// Inspect direct imports in this package only, including platform-specific
// files. Test imports may cross boundaries to exercise integration behavior.
func inspectDependencies(root string, rule dependencyRule) ([]dependencyViolation, error) {
	entries, err := os.ReadDir(filepath.Join(root, rule.packagePath))
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]bool, len(rule.allowed))
	for _, dependency := range rule.allowed {
		allowed[repositoryModule+"/"+dependency] = true
	}
	var violations []dependencyViolation
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		relativePath := filepath.Join(rule.packagePath, entry.Name())
		positions := token.NewFileSet()
		file, err := parser.ParseFile(positions, filepath.Join(root, relativePath), nil, parser.ImportsOnly)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", relativePath, err)
		}
		for _, spec := range file.Imports {
			dependency, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return nil, fmt.Errorf("parse import in %s: %w", relativePath, err)
			}
			isRepositoryDependency := dependency == repositoryModule || strings.HasPrefix(dependency, repositoryModule+"/")
			if !isRepositoryDependency || allowed[dependency] {
				continue
			}
			violations = append(violations, dependencyViolation{
				file:       filepath.ToSlash(relativePath),
				line:       positions.Position(spec.Pos()).Line,
				dependency: strings.TrimPrefix(dependency, repositoryModule+"/"),
			})
		}
	}
	return violations, nil
}

func TestDependencyScanner(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "harness")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"allowed.go":          "package harness\nimport (\n\t\"context\"\n\t_ \"" + repositoryModule + "/internal/llm\"\n\t_ \"" + repositoryModule + "-extra/client\"\n)\n",
		"forbidden.go":        "package harness\n\nimport _ \"" + repositoryModule + "/internal/config\"\n",
		"integration_test.go": "package harness\nimport _ \"" + repositoryModule + "/internal/cli\"\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	violations, err := inspectDependencies(root, dependencyRule{"internal/harness", []string{"internal/llm"}})
	if err != nil {
		t.Fatal(err)
	}
	want := dependencyViolation{"internal/harness/forbidden.go", 3, "internal/config"}
	if len(violations) != 1 || violations[0] != want {
		t.Fatalf("violations = %#v, want [%#v]", violations, want)
	}
	if err := os.WriteFile(filepath.Join(dir, "malformed.go"), []byte("package harness\nimport ("), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectDependencies(root, dependencyRule{"internal/harness", nil}); err == nil {
		t.Fatal("malformed imports must fail the scan")
	}
}
