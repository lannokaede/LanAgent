package acceptance

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/lannokaede/LanAgent/internal/config"
	"github.com/lannokaede/LanAgent/internal/llm"
)

func (v *verifier) direct(ctx context.Context, cfg *config.Config) (map[string]any, error) {
	return v.withSession(cfg, "direct", "rules", func(sess *session) (map[string]any, error) {
		answer, err := sess.run(ctx, "", "不使用工具：只回答 READY\n不使用工具：原样复述你上一条的回答\n/exit\n")
		if err != nil {
			return nil, err
		}
		rows, err := sess.journal()
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
	})
}
func (v *verifier) hybrid(ctx context.Context, cfg *config.Config) (map[string]any, error) {
	return v.withSession(cfg, "hybrid", "hybrid", func(sess *session) (map[string]any, error) {
		_, err := sess.run(ctx, "Go channel 的发送与接收有什么联系？请用一句话解释。", "")
		if err != nil {
			return nil, err
		}
		rows, err := sess.journal()
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
	})
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
	return v.withSession(&modified, "recovery", "rules", func(sess *session) (map[string]any, error) {
		if err := os.WriteFile(filepath.Join(sess.workspace, "evidence.txt"), []byte(marker), 0600); err != nil {
			return nil, err
		}
		prompt := "读取 evidence.txt，只读取一次并原样返回其中的标识；已有工具结果时直接复用，不要再次读取。"
		_, err := sess.run(ctx, prompt, "")
		if err == nil {
			return nil, errors.New("injected persistent outage did not stop execution")
		}
		before, e := sess.journal()
		if e != nil {
			return nil, e
		}
		reads := countTools(before, "read_file")
		if reads != 1 || injected.Load() != int64(cfg.LLM.Retry.MaxAttempts) {
			return metrics(before), errors.New("outage did not preserve one completed read and exhaust bounded retries")
		}
		allow.Store(true)
		answer, err := sess.run(ctx, "", "", "-resume")
		if err != nil {
			return nil, err
		}
		after, err := sess.journal()
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
	})
}
func (v *verifier) summary(ctx context.Context, cfg *config.Config) (map[string]any, error) {
	return v.withSession(cfg, "summary", "rules", func(sess *session) (map[string]any, error) {
		raw, err := os.ReadFile(filepath.Join(sess.profile, "config.local.yaml"))
		if err != nil {
			return nil, err
		}
		var profile map[string]any
		json.Unmarshal(raw, &profile)
		profile["context"] = map[string]any{"window": 8000, "output_reserve": 512, "tool_reserve": 256, "summary_tokens": 128}
		raw, _ = json.Marshal(profile)
		if err = os.WriteFile(filepath.Join(sess.profile, "config.local.yaml"), raw, 0600); err != nil {
			return nil, err
		}
		snapshot := map[string]any{"version": 1, "workspace": sess.workspace, "messages": []llm.Message{{Role: llm.RoleUser, Content: strings.Repeat("Earlier discussion: use context cancellation to stop worker goroutines. ", 160)}, {Role: llm.RoleAssistant, Content: "The earlier discussion selected context cancellation."}}}
		raw, _ = json.Marshal(snapshot)
		if err = os.WriteFile(filepath.Join(sess.state, "acceptance.json"), raw, 0600); err != nil {
			return nil, err
		}
		_, err = sess.run(ctx, "不使用工具：用一句话说明我们此前选择的取消方式。", "")
		if err != nil {
			return nil, err
		}
		rows, err := sess.journal()
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
	})
}
