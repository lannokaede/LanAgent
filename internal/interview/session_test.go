package interview

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lannokaede/LanAgent/internal/llm"
	"github.com/lannokaede/LanAgent/internal/rag"
)

func TestFollowupResumesAndBuildsWeaknessReview(t *testing.T) {
	search := searchFunc(func(context.Context, string, string, int) ([]rag.Hit, error) {
		return []rag.Hit{{Source: "https://go.dev/ref/mem", Text: "channel synchronization", Index: 1}}, nil
	})
	calls := 0
	provider := providerFunc(func(_ context.Context, r *llm.ChatRequest) (*llm.ChatResponse, error) {
		calls++
		if calls == 1 {
			return &llm.ChatResponse{Content: `{"score":6,"feedback":"补充可见性关系","references":[1],"dimensions":{"knowledge":6,"completeness":5,"expression":8},"follow_up":"请具体解释发送与接收的可见性关系？"}`}, nil
		}
		if !strings.Contains(r.Messages[1].Content, "first candidate answer") {
			t.Fatal("follow-up lost previous answer")
		}
		return &llm.ChatResponse{Content: `{"score":9,"feedback":"关系解释完整","references":[1],"follow_up":"should not cause another followup"}`}, nil
	})
	c, e := New(search, provider, "go-backend-interview", "concurrency")
	if e != nil {
		t.Fatal(e)
	}
	c.WithFollowups(true)
	q, e := c.Next(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.Answer(context.Background(), "first candidate answer"); e != nil {
		t.Fatal(e)
	}
	follow, e := c.Next(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if follow.ID != q.ID+"-followup" {
		t.Fatal("did not ask adaptive follow-up")
	}
	path := filepath.Join(t.TempDir(), "session.json")
	if e = c.Save(path); e != nil {
		t.Fatal(e)
	}
	resumed, _ := New(search, provider, "go-backend-interview", "concurrency")
	resumed.WithFollowups(true)
	if e = resumed.Load(path); e != nil {
		t.Fatal(e)
	}
	again, e := resumed.Next(context.Background())
	if e != nil || again != follow {
		t.Fatal("changed pending question")
	}
	if _, e = resumed.Answer(context.Background(), "second answer"); e != nil {
		t.Fatal(e)
	}
	next, e := resumed.Next(context.Background())
	if e != nil || next.ID != Questions("concurrency")[1].ID {
		t.Fatal("follow-up advanced/skipped wrong base question")
	}
	weak := resumed.Weaknesses()
	if len(weak) != 1 || len(weak[0].Sources) != 1 {
		t.Fatal("missing weakness provenance")
	}
	review := resumed.ReviewMarkdown()
	if !strings.Contains(review, "补充可见性关系") || !strings.Contains(review, "https://go.dev/ref/mem#1") {
		t.Fatal("missing study report")
	}
}
