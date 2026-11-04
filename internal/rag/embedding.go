package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/lannokaede/LanAgent/internal/engineering"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tmc/langchaingo/embeddings"
)

// 模块 1：实现 LangChainGo Embedder 契约。聊天模型与向量模型是独立依赖。
// 固定使用稠密 float 向量；模型、维度和处理策略共同组成索引身份。
type EmbeddingClient struct {
	baseURL, model, key string
	dimensions          int
	http                *http.Client
}

var _ embeddings.Embedder = (*EmbeddingClient)(nil)

func NewEmbeddingClient(baseURL, model, key string, dimensions int) (*EmbeddingClient, error) {
	if err := validateURL(baseURL); err != nil {
		return nil, err
	}
	if model != "text-embedding-v4" || strings.TrimSpace(key) == "" || dimensions <= 0 {
		return nil, fmt.Errorf("embedding requires text-embedding-v4, API key and positive dimensions")
	}
	return &EmbeddingClient{strings.TrimRight(baseURL, "/"), model, key, dimensions, &http.Client{Timeout: 30 * time.Second, Transport: engineering.Transport(), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func validateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("invalid service URL")
	}
	return nil
}
func (c *EmbeddingClient) Identity() string {
	return fmt.Sprintf("demo-rag-v1:%s:%s:%d:raw-text", c.baseURL, c.model, c.dimensions)
}
func (c *EmbeddingClient) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	vectors, err := c.EmbedDocuments(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

// 模块 2：百炼单批最多 10 条。先检查所有文本再请求，避免无效输入产生部分计费。
// 8192 UTF-8 字节是本工程的保守输入限制，不表示精确 Token 计数。
// 模型仍执行最终 Token 限制；超限返回错误，不静默截断文本。
func (c *EmbeddingClient) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	for _, s := range texts {
		if strings.TrimSpace(s) == "" || len(s) > 8192 {
			return nil, fmt.Errorf("embedding text must be non-empty and at most 8192 UTF-8 bytes")
		}
	}
	result := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += 10 {
		end := min(start+10, len(texts))
		payload := map[string]any{"model": c.model, "input": texts[start:end], "dimensions": c.dimensions, "encoding_format": "float"}
		var response struct {
			Data []struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
		}
		if err := postJSON(ctx, c.http, c.baseURL+"/embeddings", c.key, payload, &response); err != nil {
			return nil, fmt.Errorf("embedding: %w", err)
		}
		if len(response.Data) != end-start {
			return nil, fmt.Errorf("embedding response count mismatch")
		}
		batch := make([][]float32, end-start)
		for _, item := range response.Data {
			if item.Index < 0 || item.Index >= len(batch) || batch[item.Index] != nil {
				return nil, fmt.Errorf("invalid embedding response index")
			}
			if err := validateVector(item.Embedding, c.dimensions); err != nil {
				return nil, err
			}
			batch[item.Index] = item.Embedding
		}
		result = append(result, batch...)
	}
	return result, nil
}
func validateVector(vector []float32, dim int) error {
	if len(vector) != dim {
		return fmt.Errorf("vector dimension mismatch")
	}
	nonzero := false
	for _, v := range vector {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return fmt.Errorf("non-finite vector")
		}
		nonzero = nonzero || v != 0
	}
	if !nonzero {
		return fmt.Errorf("zero vector")
	}
	return nil
}

// 模块 3：有界 HTTP。鉴权只放请求头，错误不返回厂商原始正文或密钥。
func postJSON(ctx context.Context, client *http.Client, endpoint, key string, payload, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("service request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("service returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20+1))
	if err != nil {
		return fmt.Errorf("read service response failed")
	}
	if len(data) > 4<<20 {
		return fmt.Errorf("service response exceeds limit")
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("invalid service JSON response")
	}
	return nil
}

func (c *EmbeddingClient) Close() { c.http.CloseIdleConnections() }
