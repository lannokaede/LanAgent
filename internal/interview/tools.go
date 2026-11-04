package interview

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/lannokaede/LanAgent/internal/llm"
	"github.com/lannokaede/LanAgent/internal/tool"
)

type training struct {
	Topic       string          `json:"topic"`
	Limit       int             `json:"limit"`
	IssuedInput string          `json:"issued_input"`
	Coach       json.RawMessage `json:"coach,omitempty"`
}
type receipt struct {
	Arguments string `json:"arguments"`
	Result    string `json:"result"`
}
type trainingState struct {
	Version   int                 `json:"version"`
	Binding   string              `json:"binding"`
	Active    string              `json:"active"`
	Trainings map[string]training `json:"trainings"`
	Receipts  map[string]receipt  `json:"receipts"`
	Answers   map[string]string   `json:"answers"`
}

// Tools is owned by the same exclusive conversation lock as personal.Session.
// Original user input is bound by the host; model arguments contain only refs.
type Tools struct {
	mu             sync.Mutex
	path, kb       string
	search         Searcher
	provider       llm.Provider
	state          trainingState
	input, inputID string
}

func OpenTools(path, kb, binding string, search Searcher, provider llm.Provider) (*Tools, error) {
	t := &Tools{path: path, kb: kb, search: search, provider: provider, state: trainingState{Version: 1, Binding: binding, Trainings: map[string]training{}, Receipts: map[string]receipt{}, Answers: map[string]string{}}}
	f, e := os.Open(path)
	if errors.Is(e, os.ErrNotExist) {
		return t, nil
	}
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, 8<<20+1))
	if e != nil || len(b) > 8<<20 {
		return nil, errors.New("invalid training state size")
	}
	if json.Unmarshal(b, &t.state) != nil || t.state.Version != 1 || t.state.Binding != binding || t.state.Trainings == nil || t.state.Receipts == nil || t.state.Answers == nil {
		return nil, errors.New("training state profile differs or is corrupt")
	}
	for _, saved := range t.state.Trainings {
		if _, e := t.coach(saved); e != nil {
			return nil, e
		}
	}
	return t, nil
}
func (t *Tools) BindInput(id, text string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.inputID = id
	t.input = text
}
func (t *Tools) coach(saved training) (*Coach, error) {
	c, e := New(t.search, t.provider, t.kb, saved.Topic)
	if e != nil {
		return nil, e
	}
	c.WithFollowups(true)
	if len(saved.Coach) > 0 && string(saved.Coach) != "null" {
		e = c.Restore(saved.Coach)
	}
	return c, e
}
func (t *Tools) Register(r *tool.Registry) error {
	for _, name := range []string{"interview_start", "interview_status", "interview_next", "interview_submit_answer", "review_list"} {
		if e := r.Register(trainingTool{t, name}); e != nil {
			return e
		}
	}
	return nil
}

type trainingTool struct {
	owner *Tools
	name  string
}

func (t trainingTool) Spec() llm.ToolDef {
	props := map[string]any{}
	required := []string{}
	description := ""
	switch t.name {
	case "interview_start":
		description = "Start or resume a Go backend interview inside this conversation. Choose topic from the user request. Existing unfinished topic is resumed; one question at a time."
		props["topic"] = map[string]any{"type": "string", "enum": []string{"all", "go", "concurrency", "runtime", "database", "redis", "engineering"}}
		props["questions"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 16}
		required = []string{"topic"}
	case "interview_status":
		description = "Inspect saved interview progress and the host-bound current input_ref before processing a user answer. Does not advance the interview."
	case "interview_next":
		description = "Return the pending question, or retrieve evidence and issue one new question. No reference answer is disclosed. Explanations and research can happen before the user answers."
	case "interview_submit_answer":
		description = "Review the HOST's exact current user input for the pending question. Pass question_id and input_ref from interview_status. No model-generated answer argument is accepted. Advance only when the user actually answers."
		props["question_id"] = map[string]any{"type": "string"}
		props["input_ref"] = map[string]any{"type": "string"}
		required = []string{"question_id", "input_ref"}
	case "review_list":
		description = "Read weaknesses and practice feedback from saved interviews in this conversation. Does not advance questions."
	}
	return llm.ToolDef{Name: t.name, Description: description, Parameters: map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}}
}
func (t trainingTool) Execute(ctx context.Context, raw string) (string, error) {
	return t.owner.execute(ctx, t.name, raw)
}
func (t *Tools) execute(ctx context.Context, name, raw string) (string, error) {
	if len(raw) > 4096 {
		return "", errors.New("training arguments too large")
	}
	var args struct {
		Topic      string `json:"topic"`
		Questions  int    `json:"questions"`
		QuestionID string `json:"question_id"`
		InputRef   string `json:"input_ref"`
	}
	var provided map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &provided) != nil || provided == nil {
		return "", errors.New("arguments must be one JSON object")
	}
	allowed := trainingTool{t, name}.Spec().Parameters["properties"].(map[string]any)
	for key := range provided {
		if _, ok := allowed[key]; !ok {
			return "", fmt.Errorf("unexpected training argument %s", key)
		}
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(&args); e != nil {
		return "", e
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return "", errors.New("invalid training arguments")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if e := ctx.Err(); e != nil {
		return "", e
	}
	if name == "interview_status" {
		return t.status(t.state)
	}
	if name == "review_list" {
		out := []Weakness{}
		for _, saved := range t.state.Trainings {
			c, e := t.coach(saved)
			if e != nil {
				return "", e
			}
			out = append(out, c.Weaknesses()...)
		}
		return encodeTraining(map[string]any{"weaknesses": out})
	}
	id := tool.ExecutionID(ctx)
	if id == "" {
		return "", errors.New("training mutation requires a host execution id")
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(name+"\x00"+raw)))
	if old, ok := t.state.Receipts[id]; ok {
		if old.Arguments != fingerprint {
			return "", errors.New("training execution arguments differ")
		}
		return old.Result, nil
	}
	if len(t.state.Receipts) >= 512 {
		return "", errors.New("training receipt capacity reached")
	}
	b, _ := json.Marshal(t.state)
	var next trainingState
	if e := json.Unmarshal(b, &next); e != nil {
		return "", e
	}
	if name == "interview_start" {
		if len(Questions(args.Topic)) == 0 || args.Questions < 0 || args.Questions > 16 {
			return "", errors.New("invalid interview topic or question count")
		}
		if args.Questions == 0 {
			args.Questions = 5
		}
		next.Active = ""
		for key, saved := range next.Trainings {
			if saved.Topic == args.Topic {
				c, e := t.coach(saved)
				if e != nil {
					return "", e
				}
				if len(c.History) < saved.Limit && !(c.index >= len(c.questions) && c.followup == nil) {
					next.Active = key
					break
				}
			}
		}
		if next.Active == "" {
			if len(next.Trainings) >= 32 {
				return "", errors.New("training session capacity reached")
			}
			next.Active = id
			next.Trainings[id] = training{Topic: args.Topic, Limit: args.Questions}
		}
	} else {
		saved, ok := next.Trainings[next.Active]
		if !ok {
			return "", errors.New("start an interview first")
		}
		c, e := t.coach(saved)
		if e != nil {
			return "", e
		}
		switch name {
		case "interview_next":
			if len(c.History) >= saved.Limit {
				return t.status(next)
			}
			wasPending := c.pending != nil
			_, e = c.Next(ctx)
			if errors.Is(e, io.EOF) {
				return t.status(next)
			}
			if e != nil {
				return "", e
			}
			if !wasPending {
				saved.IssuedInput = t.inputID
			}
		case "interview_submit_answer":
			if prior, ok := next.Answers[t.inputID]; ok {
				return t.commit(next, id, fingerprint, prior)
			}
			if args.InputRef != t.inputID || t.inputID == "" {
				return "", errors.New("input_ref is stale; call interview_status for the current user turn")
			}
			if c.pending == nil || args.QuestionID != c.current().ID || saved.IssuedInput == t.inputID {
				return "", errors.New("answer requires a subsequent real user turn for the pending question")
			}
			round, e := c.Answer(ctx, t.input)
			if e != nil {
				return "", e
			}
			sources := []map[string]any{}
			for _, ref := range round.Evaluation.References {
				if ref >= 1 && ref <= len(round.Evidence) {
					hit := round.Evidence[ref-1]
					sources = append(sources, map[string]any{"source": hit.Source, "chunk_index": hit.Index})
				}
			}
			result, e := encodeTraining(map[string]any{"status": "reviewed", "question_id": round.Question.ID, "evaluation": round.Evaluation, "sources": sources, "completed": len(c.History), "limit": saved.Limit})
			if e != nil {
				return "", e
			}
			saved.Coach, e = c.Snapshot()
			if e != nil {
				return "", e
			}
			next.Trainings[next.Active] = saved
			next.Answers[t.inputID] = result
			return t.commit(next, id, fingerprint, result)
		default:
			return "", errors.New("unknown training tool")
		}
		saved.Coach, e = c.Snapshot()
		if e != nil {
			return "", e
		}
		next.Trainings[next.Active] = saved
	}
	result, e := t.status(next)
	if e != nil {
		return "", e
	}
	return t.commit(next, id, fingerprint, result)
}
func (t *Tools) status(st trainingState) (string, error) {
	saved, ok := st.Trainings[st.Active]
	if !ok {
		return encodeTraining(map[string]any{"status": "not_started"})
	}
	c, e := t.coach(saved)
	if e != nil {
		return "", e
	}
	status := "ready"
	if len(c.History) >= saved.Limit || c.index >= len(c.questions) && c.followup == nil {
		status = "completed"
	} else if c.pending != nil {
		status = "waiting_for_answer"
	}
	out := map[string]any{"status": status, "training_id": st.Active, "topic": saved.Topic, "completed": len(c.History), "limit": saved.Limit}
	if status == "waiting_for_answer" {
		q := c.current()
		out["question"] = map[string]any{"id": q.ID, "topic": q.Topic, "text": q.Text}
		out["input_ref"] = t.inputID
		out["answer_eligible"] = t.inputID != "" && t.inputID != saved.IssuedInput
	}
	return encodeTraining(out)
}
func (t *Tools) commit(next trainingState, id, args, result string) (string, error) {
	next.Receipts[id] = receipt{args, result}
	b, e := json.Marshal(next)
	if e != nil {
		return "", e
	}
	if len(b) > 8<<20 {
		return "", errors.New("training state too large")
	}
	f, e := os.CreateTemp(filepath.Dir(t.path), ".training-*.tmp")
	if e != nil {
		return "", e
	}
	defer os.Remove(f.Name())
	_, e = f.Write(b)
	e = errors.Join(e, f.Sync(), f.Close())
	if e != nil {
		return "", e
	}
	if e = os.Rename(f.Name(), t.path); e != nil {
		return "", e
	}
	t.state = next
	dir, err := os.Open(filepath.Dir(t.path))
	if err != nil {
		return "", fmt.Errorf("%w: sync training directory", tool.ErrUncertain)
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return "", fmt.Errorf("%w: sync training directory", tool.ErrUncertain)
	}
	return result, nil
}
func encodeTraining(v any) (string, error) { b, e := json.Marshal(v); return string(b), e }

// Reconcile returns an atomically saved result without repeating scoring or progress changes.
func (t *Tools) Reconcile(id, name, raw string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.state.Receipts[id]
	want := fmt.Sprintf("%x", sha256.Sum256([]byte(name+"\x00"+raw)))
	if !ok || r.Arguments != want {
		return "", errors.New("no matching committed training receipt; outcome remains uncertain")
	}
	return r.Result, nil
}
