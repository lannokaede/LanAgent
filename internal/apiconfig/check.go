package apiconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/lannokaede/LanAgent/internal/config"
	"github.com/lannokaede/LanAgent/internal/engineering"
	"github.com/lannokaede/LanAgent/internal/llm"
	"github.com/lannokaede/LanAgent/internal/rag"
)

func Models(ctx context.Context, e config.APIEndpoint) ([]string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, e.BaseURL+"/models", nil)
	if err != nil {
		return nil, errors.New("cannot construct model listing request")
	}
	request.Header.Set("Authorization", "Bearer "+e.APIKey)
	transport := engineering.Transport()
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 15 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("cannot reach model listing endpoint")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("model listing returned HTTP %d", response.StatusCode)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, errors.New("model listing exceeds size limit")
	}
	if json.Unmarshal(raw, &payload) != nil {
		return nil, errors.New("invalid model listing response")
	}
	names := []string{}
	seen := map[string]bool{}
	for _, item := range payload.Data {
		if item.ID != "" && len(item.ID) <= 200 && !seen[item.ID] {
			names = append(names, item.ID)
			seen[item.ID] = true
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, errors.New("model listing is empty")
	}
	return names, nil
}

func Check(ctx context.Context, e config.APIEndpoint, kind string) error {
	if kind == "embedding" {
		client, err := rag.NewEmbeddingClient(e.BaseURL, e.Model, e.APIKey, e.Dimensions)
		if err != nil {
			return err
		}
		_, err = client.EmbedQuery(ctx, "连接测试")
		return err
	}
	provider, err := llm.NewOpenAIProvider(e.BaseURL, e.Model, e.APIKey)
	if err != nil {
		return errors.New("invalid chat API configuration")
	}
	defer provider.Close()
	provider.SetTimeout(30 * time.Second)
	response, err := provider.StreamChat(ctx, &llm.ChatRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: "请仅回复 OK，不要解释。"}}, MaxTokens: 64}, func(string) error { return nil })
	if err != nil {
		var upstream *llm.UpstreamError
		if errors.As(err, &upstream) {
			return fmt.Errorf("聊天 API 返回 HTTP %d", upstream.StatusCode)
		}
		if ctx.Err() != nil {
			return errors.New("API 连通性测试已取消或超时")
		}
		return errors.New("聊天 API 请求失败，请检查网络、地址和模型名")
	}
	if response == nil || strings.TrimSpace(response.Content) == "" {
		return errors.New("API 未返回有效正文，请检查模型能力或输出预算")
	}
	return nil
}
