package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	assets "github.com/lannokaede/LanAgent/examples/practice"
	"github.com/lannokaede/LanAgent/internal/cli"
	"github.com/lannokaede/LanAgent/internal/config"
	"github.com/lannokaede/LanAgent/internal/llm"
	"github.com/lannokaede/LanAgent/internal/personalmemory"
	"github.com/lannokaede/LanAgent/internal/rag"
)

func (v *verifier) live(ctx context.Context) {
	cfg, err := config.Load(v.config)
	if err != nil {
		name := "live_configuration"
		if v.only != "" {
			name = v.only
		}
		v.run(name, "live", func() (map[string]any, error) { return nil, err })
		return
	}
	if v.milvus != "" {
		cfg.RAG.MilvusURL = v.milvus
	}
	for _, test := range []struct {
		name string
		fn   func(context.Context, *config.Config) (map[string]any, error)
	}{
		{"live_multi_turn_direct", v.direct},
		{"live_hybrid_classification", v.hybrid},
		{"live_workspace_and_recovery", v.recovery},
		{"live_summary", v.summary},
		{"live_memory_and_rag", v.vectors},
		{"live_model_docker", v.sandbox},
	} {
		if ctx.Err() != nil {
			if v.only == "" || v.only == test.name {
				v.report.Success = false
				v.report.Checks = append(v.report.Checks, check{Name: test.name, Mode: "live", Status: "not_run", Error: "verification canceled before this scenario"})
				fmt.Println(test.name + ": not_run")
			}
			continue
		}
		v.run(test.name, "live", func() (map[string]any, error) {
			call, cancel := context.WithTimeout(ctx, 4*time.Minute)
			defer cancel()
			return test.fn(call, cfg)
		})
	}
}
func (v *verifier) profile(cfg *config.Config, name, mode string) (string, string, string, error) {
	base := filepath.Join(v.work, name)
	for _, dir := range []string{base, filepath.Join(base, "workspace"), filepath.Join(base, "state")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return "", "", "", err
		}
	}
	values := map[string]any{
		"llm":       map[string]any{"base_url": cfg.LLM.BaseURL, "model": cfg.LLM.Model, "api_key": cfg.LLM.APIKey, "timeout": "60s", "concurrency": cfg.LLM.Concurrency, "retry": map[string]any{"max_attempts": cfg.LLM.Retry.MaxAttempts, "base_delay": cfg.LLM.Retry.BaseDelay.String()}},
		"routing":   map[string]any{"mode": mode, "timeout": v.classifierTimeout.String(), "classifier": map[string]any{"base_url": cfg.Routing.Classifier.BaseURL, "model": cfg.Routing.Classifier.Model, "api_key": cfg.Routing.Classifier.APIKey}},
		"rag":       map[string]any{"enabled": false},
		"practice":  map[string]any{"image": cfg.Practice.Image, "work_dir": filepath.Join(base, "practice")},
		"telemetry": map[string]any{"enabled": true, "file": filepath.Join(base, "spans.jsonl")},
		"context":   map[string]any{"window": 32768, "output_reserve": 4096, "tool_reserve": 2048, "summary_tokens": 512},
	}
	raw, _ := json.Marshal(values)
	if err := os.WriteFile(filepath.Join(base, "config.yaml"), []byte("rag:\n  enabled: false\n"), 0600); err != nil {
		return "", "", "", err
	}
	if err := os.WriteFile(filepath.Join(base, "config.local.yaml"), raw, 0600); err != nil {
		return "", "", "", err
	}
	return base, filepath.Join(base, "workspace"), filepath.Join(base, "state"), nil
}
func runCLI(ctx context.Context, profile, work, state, prompt, input string, extra ...string) (string, error) {
	args := []string{"-config", profile, "-workspace", work, "-state-dir", state, "-session", "acceptance", "-timeout", "150s"}
	if prompt != "" {
		args = append(args, "-prompt", prompt)
	}
	args = append(args, extra...)
	var out, status bytes.Buffer
	err := cli.Run(ctx, args, strings.NewReader(input), &out, &status)
	// Keep detailed outputs private, out of the portable metrics report.
	os.WriteFile(filepath.Join(profile, "cli.log"), append(append(out.Bytes(), '\n'), status.Bytes()...), 0600)
	return out.String(), err
}
func removeProfile(profile string) { _ = os.Remove(filepath.Join(profile, "config.local.yaml")) }
func journal(state string) ([]map[string]any, error) {
	raw, err := os.ReadFile(filepath.Join(state, "acceptance.jsonl"))
	if err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var row map[string]any
		if json.Unmarshal(line, &row) != nil {
			return nil, errors.New("invalid journal")
		}
		rows = append(rows, row)
	}
	return rows, nil
}
func metrics(rows []map[string]any) map[string]any {
	attempts, retries, known, unknown := 0, 0, 0, 0
	prompt, completion := float64(0), float64(0)
	for _, row := range rows {
		e, _ := row["event"].(map[string]any)
		data, _ := e["Data"].(map[string]any)
		switch e["Type"] {
		case "model_attempt_started":
			attempts++
		case "model_retry":
			retries++
		case "model_attempt_completed":
			if usage, ok := data["usage"].(map[string]any); ok {
				known++
				p, _ := usage["prompt_tokens"].(float64)
				c, _ := usage["completion_tokens"].(float64)
				prompt += p
				completion += c
			} else {
				unknown++
			}
		}
	}
	return map[string]any{"model_attempts": attempts, "retries": retries, "usage_known_attempts": known, "usage_unknown_attempts": unknown, "reported_prompt_tokens": prompt, "reported_completion_tokens": completion}
}
func countTools(rows []map[string]any, name string) int {
	n := 0
	for _, row := range rows {
		e, _ := row["event"].(map[string]any)
		d, _ := e["Data"].(map[string]any)
		if e["Type"] == "tool_call" && d["name"] == name {
			n++
		}
	}
	return n
}
func (v *verifier) direct(ctx context.Context, cfg *config.Config) (map[string]any, error) {
	p, w, s, err := v.profile(cfg, "direct", "rules")
	if err != nil {
		return nil, err
	}
	defer removeProfile(p)
	answer, err := runCLI(ctx, p, w, s, "", "不使用工具：只回答 READY\n不使用工具：原样复述你上一条的回答\n/exit\n")
	if err != nil {
		return nil, err
	}
	rows, err := journal(s)
	if err != nil {
		return nil, err
	}
	m := metrics(rows)
	direct := 0
	for _, row := range rows {
		e, _ := row["event"].(map[string]any)
		d, _ := e["Data"].(map[string]any)
		if e["Type"] == "route_selected" && d["route"] == "direct" {
			direct++
		}
		if e["Type"] == "tool_call" {
			return m, errors.New("direct mode executed a tool")
		}
	}
	m["direct_turns"] = direct
	m["chat_model"] = cfg.LLM.Model
	if direct != 2 || strings.Count(answer, "READY") < 2 {
		return m, errors.New("multi-turn direct reply failed")
	}
	return m, nil
}
func (v *verifier) hybrid(ctx context.Context, cfg *config.Config) (map[string]any, error) {
	p, w, s, err := v.profile(cfg, "hybrid", "hybrid")
	if err != nil {
		return nil, err
	}
	defer removeProfile(p)
	_, err = runCLI(ctx, p, w, s, "Go channel 的发送与接收有什么联系？请用一句话解释。", "")
	if err != nil {
		return nil, err
	}
	rows, err := journal(s)
	if err != nil {
		return nil, err
	}
	m := metrics(rows)
	m["classifier_model"] = cfg.Routing.Classifier.Model
	m["classifier_timeout_seconds"] = v.classifierTimeout.Seconds()
	for _, row := range rows {
		e, _ := row["event"].(map[string]any)
		d, _ := e["Data"].(map[string]any)
		if e["Type"] == "route_selected" {
			m["route"], m["source"] = d["route"], d["source"]
			if d["route"] == "direct" && d["source"] == "classifier" {
				return m, nil
			}
		}
	}
	return m, errors.New("real classifier did not produce a direct decision; conservative fallback remains active")
}
func (v *verifier) recovery(ctx context.Context, cfg *config.Config) (map[string]any, error) {
	target, err := url.Parse(cfg.LLM.BaseURL)
	if err != nil {
		return nil, err
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) { http.Error(w, "upstream unavailable", 502) }
	var allow atomic.Bool
	var injected atomic.Int64
	marker := "harness_evidence_" + rand.Text()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			w.WriteHeader(400)
			return
		}
		r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var body struct{ Messages []llm.Message }
		json.Unmarshal(raw, &body)
		for _, message := range body.Messages {
			if message.Role == llm.RoleTool && strings.Contains(message.Content, marker) && !allow.Load() {
				injected.Add(1)
				w.WriteHeader(503)
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	defer server.Close()
	modified := *cfg
	modified.LLM.BaseURL = server.URL
	p, w, s, err := v.profile(&modified, "recovery", "rules")
	if err != nil {
		return nil, err
	}
	defer removeProfile(p)
	if err = os.WriteFile(filepath.Join(w, "evidence.txt"), []byte(marker), 0600); err != nil {
		return nil, err
	}
	prompt := "读取 evidence.txt，只读取一次并原样返回其中的标识；已有工具结果时直接复用，不要再次读取。"
	_, err = runCLI(ctx, p, w, s, prompt, "")
	if err == nil {
		return nil, errors.New("injected persistent outage did not stop execution")
	}
	before, e := journal(s)
	if e != nil {
		return nil, e
	}
	reads := countTools(before, "read_file")
	if reads != 1 || injected.Load() != int64(cfg.LLM.Retry.MaxAttempts) {
		return metrics(before), errors.New("outage did not preserve one completed read and exhaust bounded retries")
	}
	allow.Store(true)
	answer, err := runCLI(ctx, p, w, s, "", "", "-resume")
	if err != nil {
		return nil, err
	}
	after, err := journal(s)
	if err != nil {
		return nil, err
	}
	m := metrics(after)
	m["read_calls_before"] = reads
	m["read_calls_after"] = countTools(after, "read_file")
	m["injected_http_503"] = injected.Load()
	if countTools(after, "read_file") != 1 || !strings.Contains(answer, marker) {
		return m, errors.New("resume repeated a completed tool or lost its result")
	}
	return m, nil
}
func (v *verifier) summary(ctx context.Context, cfg *config.Config) (map[string]any, error) {
	p, w, s, err := v.profile(cfg, "summary", "rules")
	if err != nil {
		return nil, err
	}
	defer removeProfile(p)
	raw, err := os.ReadFile(filepath.Join(p, "config.local.yaml"))
	if err != nil {
		return nil, err
	}
	var profile map[string]any
	json.Unmarshal(raw, &profile)
	profile["context"] = map[string]any{"window": 8000, "output_reserve": 512, "tool_reserve": 256, "summary_tokens": 128}
	raw, _ = json.Marshal(profile)
	if err = os.WriteFile(filepath.Join(p, "config.local.yaml"), raw, 0600); err != nil {
		return nil, err
	}
	snapshot := map[string]any{"version": 1, "workspace": w, "messages": []llm.Message{{Role: llm.RoleUser, Content: strings.Repeat("Earlier discussion: use context cancellation to stop worker goroutines. ", 160)}, {Role: llm.RoleAssistant, Content: "The earlier discussion selected context cancellation."}}}
	raw, _ = json.Marshal(snapshot)
	if err = os.WriteFile(filepath.Join(s, "acceptance.json"), raw, 0600); err != nil {
		return nil, err
	}
	_, err = runCLI(ctx, p, w, s, "不使用工具：用一句话说明我们此前选择的取消方式。", "")
	if err != nil {
		return nil, err
	}
	rows, err := journal(s)
	if err != nil {
		return nil, err
	}
	m := metrics(rows)
	summaries, compactions := 0, 0
	for _, row := range rows {
		e, _ := row["event"].(map[string]any)
		d, _ := e["Data"].(map[string]any)
		if e["Type"] == "model_attempt_completed" && d["purpose"] == "summary" && d["status"] == "ok" {
			summaries++
		}
		if e["Type"] == "context_compaction" {
			compactions++
		}
	}
	m["successful_summary_calls"] = summaries
	m["compactions"] = compactions
	if summaries != 1 || compactions != 1 {
		return m, errors.New("real summary or context compaction not observed")
	}
	return m, nil
}
func (v *verifier) sandbox(ctx context.Context, cfg *config.Config) (map[string]any, error) {
	p, w, s, err := v.profile(cfg, "sandbox", "rules")
	if err != nil {
		return nil, err
	}
	defer removeProfile(p)
	code, err := assets.Files.ReadFile("context-cancel/v1/correct.go.txt")
	if err != nil {
		return nil, err
	}
	prompt := "请原样使用以下 Go 代码调用 run_go_exercise，禁止修改代码；完成后报告工具返回的 code_hash 和测试状态。\n" + string(code)
	_, err = runCLI(ctx, p, w, s, prompt, "", "-exercise", "context-cancel/v1")
	if err != nil {
		return nil, err
	}
	rows, err := journal(s)
	if err != nil {
		return nil, err
	}
	m := metrics(rows)
	m["docker_image"] = cfg.Practice.Image
	m["sandbox_calls"] = countTools(rows, "run_go_exercise")
	for _, row := range rows {
		e, _ := row["event"].(map[string]any)
		d, _ := e["Data"].(map[string]any)
		if e["Type"] == "tool_result" {
			result, _ := d["result"].(string)
			var evidence map[string]any
			if json.Unmarshal([]byte(result), &evidence) == nil && evidence["status"] == "passed" && evidence["code_hash"] != "" {
				m["code_hash"] = evidence["code_hash"]
				return m, nil
			}
		}
	}
	return m, errors.New("no hash-bound passing Docker result observed")
}

func (v *verifier) vectors(ctx context.Context, cfg *config.Config) (m map[string]any, resultErr error) {
	suffix := strings.ToLower(rand.Text())
	memoryName := "harness_verify_memory_" + suffix
	ragName := "harness_verify_rag_" + suffix
	m = map[string]any{"embedding_model": cfg.Embedding.Model, "dimensions": cfg.Embedding.Dimensions, "memory_collection": memoryName, "rag_collection": ragName}
	// Cleanup touches only the two names allocated by this invocation.
	defer func() {
		finish, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var cleanupErr error
		for _, name := range []string{memoryName, ragName} {
			cleanupErr = errors.Join(cleanupErr, dropCollection(finish, cfg, name))
		}
		m["isolated_collections_cleaned"] = cleanupErr == nil
		resultErr = errors.Join(resultErr, cleanupErr)
	}()
	emb, err := rag.NewEmbeddingClient(cfg.Embedding.BaseURL, cfg.Embedding.Model, cfg.Embedding.APIKey, cfg.Embedding.Dimensions)
	if err != nil {
		return m, err
	}
	index, err := personalmemory.NewMilvus(cfg.RAG.MilvusURL, cfg.RAG.MilvusToken, memoryName, emb, cfg.Embedding.Dimensions)
	if err != nil {
		emb.Close()
		return m, err
	}
	defer index.Close()
	if err = index.Init(ctx); err != nil {
		return m, err
	}
	store, err := personalmemory.Open(ctx, filepath.Join(v.work, "acceptance-memory.db"))
	if err != nil {
		return m, err
	}
	defer store.Close()
	if err = store.BindIndex(ctx, index.Identity()); err != nil {
		return m, err
	}
	service := personalmemory.New(store, v.work, index)
	oldValue, newValue := "old_goal_"+suffix, "new_goal_"+suffix
	old, err := store.Mutate(ctx, rand.Text(), store.UserScope(), "goal", oldValue, "acceptance", 0, false)
	if err != nil {
		return m, err
	}
	if ok, e := service.ProcessOne(ctx); e != nil || !ok {
		return m, errors.New("initial memory synchronization failed")
	}
	current, err := store.Mutate(ctx, rand.Text(), store.UserScope(), "goal", newValue, "acceptance", old.Version, false)
	if err != nil {
		return m, err
	}
	if ok, e := service.ProcessOne(ctx); e != nil || !ok {
		return m, errors.New("updated memory synchronization failed")
	}
	if err = index.Sync(ctx, old); err != nil {
		return m, err
	} // Reproduce a late obsolete vector writer.
	ids, err := index.Search(ctx, []string{store.UserScope(), store.ProjectScope(v.work)}, "goal", 20)
	if err != nil || !slices.Contains(ids, old.VectorID) || !slices.Contains(ids, current.VectorID) {
		return m, errors.New("real vector search failed")
	}
	recalled, err := service.Recall(ctx, "goal")
	if err != nil || !strings.Contains(recalled, newValue) || strings.Contains(recalled, oldValue) {
		return m, errors.New("stale memory survived authoritative version filtering")
	}
	if _, err = store.Mutate(ctx, rand.Text(), store.UserScope(), "goal", "", "acceptance", current.Version, true); err != nil {
		return m, err
	}
	if ok, e := service.ProcessOne(ctx); e != nil || !ok {
		return m, errors.New("deleted memory synchronization failed")
	}
	if err = index.Sync(ctx, old); err != nil {
		return m, err
	} // Reintroduce an obsolete vector after deletion.
	ids, err = index.Search(ctx, []string{store.UserScope(), store.ProjectScope(v.work)}, "goal", 20)
	if err != nil || !slices.Contains(ids, old.VectorID) {
		return m, errors.New("deleted stale vector was not present for filter verification")
	}
	recalled, err = service.Recall(ctx, "goal")
	if err != nil || strings.Contains(recalled, oldValue) || strings.Contains(recalled, newValue) {
		return m, errors.New("deleted memory was recalled")
	}
	m["stale_and_deleted_filtered"] = true
	modified := *cfg
	modified.RAG.Collection = ragName
	modified.RAG.KnowledgeBase = "harness-acceptance"
	engine, vectorStore, err := rag.FromConfig(&modified)
	if err != nil {
		return m, err
	}
	defer vectorStore.Close()
	if closer, ok := engine.Embedder.(interface{ Close() }); ok {
		defer closer.Close()
	}
	if err = vectorStore.Init(ctx); err != nil {
		return m, err
	}
	doc, err := engine.Import(ctx, modified.RAG.KnowledgeBase, "acceptance.md", strings.NewReader("Acceptance fixture: Go context cancellation allows workers to observe the Done channel and stop work. Call cancel to release resources."))
	if err != nil {
		return m, err
	}
	hits, err := engine.Search(ctx, modified.RAG.KnowledgeBase, "Go context cancellation", 3)
	if err != nil || len(hits) == 0 {
		return m, errors.New("real RAG import/search failed")
	}
	m["rag_chunks"] = len(doc.Chunks)
	m["rag_hits"] = len(hits)
	return m, nil
}
func dropCollection(ctx context.Context, cfg *config.Config, name string) error {
	if !strings.HasPrefix(name, "harness_verify_") {
		return errors.New("refusing cleanup outside acceptance namespace")
	}
	raw, _ := json.Marshal(map[string]string{"collectionName": name})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.RAG.MilvusURL, "/")+"/v2/vectordb/collections/drop", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.RAG.MilvusToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.RAG.MilvusToken)
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	response, err := client.Do(req)
	if err != nil {
		return errors.New("acceptance collection cleanup unavailable")
	}
	defer response.Body.Close()
	var body struct{ Code *int }
	if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&body) != nil || response.StatusCode != 200 || body.Code == nil || *body.Code != 0 {
		return errors.New("acceptance collection cleanup failed")
	}
	return nil
}
