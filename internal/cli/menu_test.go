package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/lannokaede/LanAgent/internal/personal"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMenuNewConversationThenSelectWithoutLosingBufferedInput(t *testing.T) {
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"菜单测试回复\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	cfg, work, state := t.TempDir(), t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(cfg, "config.yaml"), []byte("llm:\n  timeout: 5s\n"), 0600)
	t.Setenv("LANAGENT_API_CONFIG", filepath.Join(cfg, "missing.local.json"))
	t.Setenv("LLM_API_KEY", "test-key")
	t.Setenv("LLM_MODEL", "test")
	t.Setenv("LLM_BASE_URL", server.URL)
	// New -> message -> /menu -> history -> select -> continue -> /exit -> quit.
	input := strings.NewReader("1\n这是会话标题\n/menu\n2\n1\n\n继续聊\n/exit\n0\n")
	var out, status bytes.Buffer
	if e := Run(context.Background(), []string{"-menu", "-config", cfg, "-workspace", work, "-state-dir", state}, input, &out, &status); e != nil {
		t.Fatal(e, status.String())
	}
	if count != 2 {
		t.Fatal("lost buffered input", count, status.String())
	}
	sessions, e := personal.List(state)
	if e != nil || len(sessions) != 1 || sessions[0].Launch == nil {
		t.Fatal(sessions, e)
	}
	if !strings.Contains(status.String(), "这是会话标题") || strings.Count(out.String(), "菜单测试回复") != 2 {
		t.Fatal(status.String(), out.String())
	}
}
func TestMenuRestoresExactLaunchChoices(t *testing.T) {
	p := personal.Launch{Config: "/profiles/knowledge", Workspace: "/work", MCP: "/private/mcp.json", Write: true, Commands: true, Summary: false, Rounds: 7, Timeout: 123000000000, Exercise: "context-cancel/v1"}
	args := launchArgs(p, "/states", "session", "-resume")
	b, _ := json.Marshal(args)
	for _, v := range []string{"/profiles/knowledge", "/private/mcp.json", "-allow-write", "-allow-commands", "-summary=false", "-resume"} {
		if !strings.Contains(string(b), v) {
			t.Fatal(string(b))
		}
	}
	reader := bufio.NewReader(strings.NewReader("line one\nline two\n"))
	sc := bufio.NewScanner(&lineInput{reader})
	if !sc.Scan() || sc.Text() != "line one" {
		t.Fatal("bad line")
	}
	rest, _ := reader.ReadString('\n')
	if rest != "line two\n" {
		t.Fatal("prefetched next menu choice", rest)
	}
}

func TestMenuRestoresMemoryOptions(t *testing.T) {
	disabled := false
	p := personal.Launch{Config: "configs/harness", Workspace: "/work", Summary: true, Rounds: 10, Timeout: 123000000000, Memory: &disabled, MemoryDB: "/profile/memory.db", MemoryVectors: false, MemoryCollection: "personal_test"}
	args := launchArgs(p, "/states", "session", "")
	raw, _ := json.Marshal(args)
	for _, want := range []string{"-memory=false", "/profile/memory.db", "personal_test"} {
		if !strings.Contains(string(raw), want) {
			t.Fatal(string(raw))
		}
	}
}
