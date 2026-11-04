package interview

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lannokaede/LanAgent/internal/rag"
)

type savedSession struct {
	Version  int       `json:"version"`
	Topic    string    `json:"topic"`
	KB       string    `json:"knowledge_base"`
	Index    int       `json:"index"`
	Followup *Question `json:"followup,omitempty"`
	Pending  []rag.Hit `json:"pending,omitempty"`
	History  []Round   `json:"history"`
}

// Save includes the pending question's evidence, so reopening does not change
// the question or sources after a candidate has already started thinking.
func (c *Coach) Save(path string) error {
	data, e := c.Snapshot()
	if e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".interview-*.tmp")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	_, e = f.Write(data)
	e = errors.Join(e, f.Sync(), f.Close())
	if e != nil {
		return e
	}
	return os.Rename(f.Name(), path)
}
func (c *Coach) Load(path string) error {
	data, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	return c.Restore(data)
}

// Snapshot/Restore allow the unified conversation to atomically persist both
// interview progress and tool receipts in one application-owned transaction.
func (c *Coach) Snapshot() ([]byte, error) {
	b, err := json.Marshal(savedSession{Version: 1, Topic: c.topic, KB: c.kb, Index: c.index, Followup: c.followup, Pending: c.pending, History: c.History})
	if len(b) > 8<<20 {
		return nil, errors.New("interview session too large")
	}
	return b, err
}
func (c *Coach) Restore(data []byte) error {
	if len(data) > 8<<20 {
		return errors.New("interview session too large")
	}
	var state savedSession
	if e := json.Unmarshal(data, &state); e != nil {
		return e
	}
	if state.Version != 1 || state.Topic != c.topic || state.KB != c.kb || state.Index < 0 || state.Index > len(c.questions) {
		return errors.New("interview session configuration mismatch")
	}
	if state.Index == len(c.questions) && state.Followup == nil && len(state.Pending) > 0 {
		return errors.New("unexpected question after interview end")
	}
	if state.Followup != nil {
		if state.Index == 0 || state.Followup.ID != c.questions[state.Index-1].ID+"-followup" || state.Followup.Query != c.questions[state.Index-1].Query || state.Followup.Topic != c.questions[state.Index-1].Topic {
			return errors.New("invalid pending follow-up")
		}
	}
	c.index = state.Index
	c.followup = state.Followup
	c.pending = state.Pending
	c.History = state.History
	return nil
}

type Weakness struct {
	QuestionID      string   `json:"question_id"`
	Topic           string   `json:"topic"`
	Question        string   `json:"question"`
	Feedback        string   `json:"feedback"`
	ReferenceAnswer string   `json:"reference_answer"`
	Sources         []string `json:"sources"`
}

func (c *Coach) Weaknesses() []Weakness {
	result := []Weakness{}
	for _, r := range c.History {
		weak := r.Evaluation.Score < 8
		for _, v := range r.Evaluation.Dimensions {
			if v < 7 {
				weak = true
			}
		}
		if !weak {
			continue
		}
		sources := []string{}
		seen := map[string]bool{}
		for _, ref := range r.Evaluation.References {
			if ref < 1 || ref > len(r.Evidence) {
				continue
			}
			h := r.Evidence[ref-1]
			key := fmt.Sprintf("%s#%d", h.Source, h.Index)
			if !seen[key] {
				sources = append(sources, key)
				seen[key] = true
			}
		}
		result = append(result, Weakness{r.Question.ID, r.Question.Topic, r.Question.Text, r.Evaluation.Feedback, r.Evaluation.ReferenceAnswer, sources})
	}
	return result
}
func (c *Coach) ReviewMarkdown() string {
	var out strings.Builder
	fmt.Fprintf(&out, "# Go 后端面试复盘\n\n已回答 %d 题。反馈与分数仅用于练习。\n", len(c.History))
	for _, w := range c.Weaknesses() {
		fmt.Fprintf(&out, "\n## %s：%s\n\n%s\n\n参考要点：%s\n", w.Topic, w.Question, w.Feedback, w.ReferenceAnswer)
		for _, source := range w.Sources {
			fmt.Fprintf(&out, "\n- %s\n", source)
		}
	}
	if len(c.Weaknesses()) == 0 {
		out.WriteString("\n当前记录未标记待复习题目。\n")
	}
	return out.String()
}
