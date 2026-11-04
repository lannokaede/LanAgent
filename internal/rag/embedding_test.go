package rag

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEmbeddingProtocolAndValidation(t *testing.T) {
	mode := "ok"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("incorrect embedding request")
		}
		var body struct {
			Model      string
			Input      []string
			Dimensions int
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != "text-embedding-v4" || body.Dimensions != 2 {
			t.Error("incorrect model/dimension")
		}
		switch mode {
		case "ok":
			w.Write([]byte(`{"data":[{"index":1,"embedding":[0,1]},{"index":0,"embedding":[1,0]}]}`))
		case "duplicate":
			w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]},{"index":0,"embedding":[0,1]}]}`))
		case "dimension":
			w.Write([]byte(`{"data":[{"index":0,"embedding":[1]}]}`))
		case "error":
			w.WriteHeader(401)
			w.Write([]byte(`{"error":{"message":"test-key"}}`))
		}
	}))
	defer server.Close()
	client, err := NewEmbeddingClient(server.URL, "text-embedding-v4", "test-key", 2)
	if err != nil {
		t.Fatal(err)
	}
	vectors, err := client.EmbedDocuments(context.Background(), []string{"一", "二"})
	if err != nil || vectors[0][0] != 1 || vectors[1][1] != 1 {
		t.Fatalf("ordering failed: %v", err)
	}
	for _, bad := range []string{"duplicate", "dimension", "error"} {
		mode = bad
		if _, err := client.EmbedDocuments(context.Background(), []string{"一", "二"}); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.EmbedQuery(ctx, "query"); err == nil {
		t.Fatal("ignored cancellation")
	}
}
