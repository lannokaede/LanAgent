package interview

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lannokaede/LanAgent/internal/llm"
	"github.com/lannokaede/LanAgent/internal/rag"
)

type searchFunc func(context.Context, string, string, int) ([]rag.Hit, error)

func (f searchFunc) Search(c context.Context, k, q string, n int) ([]rag.Hit, error) {
	return f(c, k, q, n)
}

type providerFunc func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error)

func (f providerFunc) Chat(c context.Context, r *llm.ChatRequest) (*llm.ChatResponse, error) {
	return f(c, r)
}

func TestInterviewWaitsForAnswerAndOnlyAdvancesOnValidFeedback(t *testing.T) {
	models, searches := 0, 0
	bad := false
	s := searchFunc(func(_ context.Context, k, q string, n int) ([]rag.Hit, error) {
		searches++
		if k != "go-backend-interview" {
			t.Fatal("wrong namespace")
		}
		return []rag.Hit{{Source: "https://go.dev/ref/mem", Index: 7, Text: "channel synchronization"}}, nil
	})
	p := providerFunc(func(_ context.Context, r *llm.ChatRequest) (*llm.ChatResponse, error) {
		models++
		if !strings.Contains(r.Messages[1].Content, "candidate answer") || !strings.Contains(r.Messages[1].Content, "channel synchronization") {
			t.Fatal("missing answer/evidence")
		}
		if bad {
			return &llm.ChatResponse{Content: `{"score":8,"feedback":"ok","reference_answer":"answer","references":[999]}`}, nil
		}
		return &llm.ChatResponse{Content: `{"score":8,"feedback":"指出正确点与遗漏","reference_answer":"参考解释","references":[1]}`}, nil
	})
	c, err := New(s, p, "go-backend-interview", "concurrency")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Answer(context.Background(), "answer"); err == nil {
		t.Fatal("accepted answer before question")
	}
	first, err := c.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	again, _ := c.Next(context.Background())
	if first.ID != again.ID || models != 0 || searches != 1 {
		t.Fatal("question exposed answer or advanced before candidate replied")
	}
	bad = true
	if _, err := c.Answer(context.Background(), "candidate answer"); err == nil || len(c.History) != 0 {
		t.Fatal("committed invalid evidence citation")
	}
	bad = false
	round, err := c.Answer(context.Background(), "candidate answer")
	if err != nil {
		t.Fatal(err)
	}
	if round.Question.ID != first.ID || len(round.Evidence) != 1 || len(c.History) != 1 {
		t.Fatal("lost provenance")
	}
	if round.Evaluation.ReferenceAnswer != referenceAnswers[first.ID] {
		t.Fatal("model replaced the server reference answer")
	}
	next, err := c.Next(context.Background())
	if err != nil || next.ID == first.ID {
		t.Fatal("did not advance exactly one question")
	}
}

func TestNoEvidenceDoesNotInventQuestionReview(t *testing.T) {
	s := searchFunc(func(context.Context, string, string, int) ([]rag.Hit, error) { return nil, nil })
	p := providerFunc(func(context.Context, *llm.ChatRequest) (*llm.ChatResponse, error) {
		t.Fatal("called model without evidence")
		return nil, nil
	})
	c, _ := New(s, p, "kb", "go")
	if _, err := c.Next(context.Background()); err == nil {
		t.Fatal("accepted empty knowledge base")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.pending = []rag.Hit{{Text: "evidence"}}
	if _, err := c.Answer(ctx, "answer"); !errors.Is(err, context.Canceled) {
		t.Fatalf("ignored cancellation: %v", err)
	}
}
