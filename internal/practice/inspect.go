package practice

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/scanner"
	"go/token"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Inspection struct {
	SyntaxValid   bool      `json:"syntax_valid"`
	ContractValid bool      `json:"contract_valid"`
	Issues        []Finding `json:"issues"`
	Functions     []string  `json:"functions"`
}

var allowedImports = map[string]bool{"context": true, "time": true, "sync": true, "errors": true, "fmt": true, "sort": true, "math": true}

func expression(n ast.Node) string {
	var b bytes.Buffer
	_ = printer.Fprint(&b, token.NewFileSet(), n)
	return b.String()
}
func funcShape(f *ast.FuncDecl) string {
	if f.Type.TypeParams != nil {
		return f.Name.Name + "[generic]"
	}
	fields := func(list *ast.FieldList) string {
		var out []string
		if list != nil {
			for _, v := range list.List {
				n := len(v.Names)
				if n == 0 {
					n = 1
				}
				for i := 0; i < n; i++ {
					out = append(out, expression(v.Type))
				}
			}
		}
		return strings.Join(out, ",")
	}
	return f.Name.Name + "(" + fields(f.Type.Params) + ")(" + fields(f.Type.Results) + ")"
}

// Policy is enforced before container execution too. Parse errors may compile-fail in the sandbox;
// unsupported imports/directives are rejected, never downloaded or run on the host.
func ExecutionPolicy(code string) error {
	if len(code) == 0 || len(code) > MaxCodeBytes || !utf8.ValidString(code) || strings.Contains(code, "//go:") {
		return ErrInput
	}
	file, _ := parser.ParseFile(token.NewFileSet(), "solution.go", code, parser.AllErrors)
	if file == nil {
		return nil
	}
	if file.Name != nil && file.Name.Name != "exercise" {
		return fmt.Errorf("package 必须为 exercise")
	}
	for _, v := range file.Imports {
		path, err := strconv.Unquote(v.Path.Value)
		if err != nil || !allowedImports[path] {
			return fmt.Errorf("仅支持练习允许的标准库，不能导入 %s", path)
		}
	}
	for _, d := range file.Decls {
		if f, ok := d.(*ast.FuncDecl); ok && f.Name.Name == "init" {
			return fmt.Errorf("练习不允许 init 函数")
		}
	}
	return nil
}
func Inspect(e Exercise, code string) Inspection {
	out := Inspection{Issues: []Finding{}, Functions: []string{}}
	file, err := parser.ParseFile(token.NewFileSet(), "solution.go", code, parser.AllErrors)
	if err != nil {
		if list, ok := err.(scanner.ErrorList); ok {
			for _, issue := range list {
				line := max(1, issue.Pos.Line)
				out.Issues = append(out.Issues, Finding{Line: line, EndLine: line, Message: issue.Msg})
			}
		} else {
			out.Issues = append(out.Issues, Finding{Line: 1, EndLine: 1, Message: err.Error()})
		}
		return out
	}
	out.SyntaxValid = true
	if err = ExecutionPolicy(code); err != nil {
		out.Issues = append(out.Issues, Finding{Line: 1, EndLine: 1, Message: err.Error()})
	}
	want, _ := parser.ParseFile(token.NewFileSet(), "contract.go", "package exercise\n"+e.Signature+" {}", 0)
	var signature string
	if want != nil {
		signature = funcShape(want.Decls[0].(*ast.FuncDecl))
	}
	for _, d := range file.Decls {
		if f, ok := d.(*ast.FuncDecl); ok {
			out.Functions = append(out.Functions, funcShape(f))
			if f.Recv == nil && funcShape(f) == signature {
				out.ContractValid = true
			}
		}
	}
	if !out.ContractValid {
		out.Issues = append(out.Issues, Finding{Line: 1, EndLine: 1, Message: "缺少约定函数：" + e.Signature})
	}
	return out
}
