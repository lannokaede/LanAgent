package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/lannokaede/LanAgent/internal/engineering"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 模块 1：Milvus REST v2 存储边界。不改变现有学习库，不使用聊天模型向量。
// 集合 description 保存模型身份；新建集合采用固定 Schema 与 COSINE 索引。
type MilvusStore struct {
	baseURL, key, collection, identity string
	dimensions                         int
	http                               *http.Client
}
type Hit struct {
	DocumentID string  `json:"document_id,omitempty"`
	RevisionID string  `json:"revision_id,omitempty"`
	ID         string  `json:"id"`
	Score      float64 `json:"distance"`
	Source     string  `json:"source"`
	Index      int     `json:"chunk_index"`
	Text       string  `json:"text"`
}

var collectionName = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]{0,127}$`)

func NewMilvusStore(baseURL, key, collection string, dimensions int, identity string) (*MilvusStore, error) {
	if err := validateURL(baseURL); err != nil {
		return nil, err
	}
	if !collectionName.MatchString(collection) || dimensions <= 0 || identity == "" {
		return nil, fmt.Errorf("invalid Milvus collection configuration")
	}
	return &MilvusStore{strings.TrimRight(baseURL, "/"), key, collection, identity, dimensions, &http.Client{Timeout: 30 * time.Second, Transport: engineering.Transport(), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (s *MilvusStore) call(ctx context.Context, route string, body map[string]any, out any) error {
	body["collectionName"] = s.collection
	var envelope struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := postJSON(ctx, s.http, s.baseURL+"/v2/vectordb/"+route, s.key, body, &envelope); err != nil {
		return err
	}
	if envelope.Code == nil {
		return fmt.Errorf("Milvus response is missing status code")
	}
	if *envelope.Code != 0 {
		return fmt.Errorf("Milvus error code %d", *envelope.Code)
	}
	if out != nil {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return fmt.Errorf("invalid Milvus data")
		}
	}
	return nil
}

// 模块 2：只有显式 init 命令建集合。聊天启动只检查已有集合，不隐式写库。
func (s *MilvusStore) Init(ctx context.Context) error {
	var status struct {
		Has bool `json:"has"`
	}
	if err := s.call(ctx, "collections/has", map[string]any{}, &status); err != nil {
		return err
	}
	if status.Has {
		return s.Check(ctx)
	}
	fields := []any{map[string]any{"fieldName": "id", "dataType": "VarChar", "isPrimary": true, "elementTypeParams": map[string]any{"max_length": "64"}}, map[string]any{"fieldName": "vector", "dataType": "FloatVector", "elementTypeParams": map[string]any{"dim": strconv.Itoa(s.dimensions)}}}
	for _, f := range []struct {
		name string
		size int
	}{{"kb", 64}, {"document_id", 64}, {"revision_id", 64}, {"source", 255}, {"text", 16384}} {
		fields = append(fields, map[string]any{"fieldName": f.name, "dataType": "VarChar", "elementTypeParams": map[string]any{"max_length": strconv.Itoa(f.size)}})
	}
	fields = append(fields, map[string]any{"fieldName": "chunk_index", "dataType": "Int64"})
	err := s.call(ctx, "collections/create", map[string]any{
		"description": s.identity, "params": map[string]any{"consistencyLevel": "Strong"},
		"schema":      map[string]any{"autoId": false, "enabledDynamicField": false, "fields": fields},
		"indexParams": []any{map[string]any{"fieldName": "vector", "indexName": "vector_idx", "metricType": "COSINE", "params": map[string]any{"index_type": "AUTOINDEX"}}},
	}, nil)
	if err != nil {
		return err
	}
	return s.Check(ctx)
}
func (s *MilvusStore) Check(ctx context.Context) error {
	var description struct {
		Description string `json:"description"`
		Fields      []struct {
			Name   string `json:"name"`
			Type   string `json:"type"`
			Params []struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			} `json:"params"`
		} `json:"fields"`
	}
	if err := s.call(ctx, "collections/describe", map[string]any{}, &description); err != nil {
		return err
	}
	if description.Description != s.identity {
		return fmt.Errorf("Milvus model identity mismatch; use a new matching collection")
	}
	for _, field := range description.Fields {
		if field.Name == "vector" && field.Type == "FloatVector" {
			for _, p := range field.Params {
				if p.Key == "dim" && p.Value == strconv.Itoa(s.dimensions) {
					return nil
				}
			}
		}
	}
	return fmt.Errorf("Milvus vector schema mismatch")
}

// 模块 3：同版本使用稳定 Chunk ID 做幂等 upsert。全部批次成功后再清理旧版本。
// 这不是事务：失败可能留下部分新版本，查询可能短暂看到两版；重新导入可修复。
// 当前约定一个知识库只允许一个导入进程；TODO：生产环境使用版本清单和原子激活。
func (s *MilvusStore) Write(ctx context.Context, doc *PreparedDocument, vectors [][]float32) error {
	if doc == nil || len(doc.Chunks) == 0 || len(doc.Chunks) != len(vectors) {
		return fmt.Errorf("invalid indexed document")
	}
	if !knowledgeBaseName.MatchString(doc.Document.KnowledgeBaseID) {
		return fmt.Errorf("invalid knowledge base")
	}
	for i, c := range doc.Chunks {
		if c.DocumentID != doc.Document.ID || c.RevisionID != doc.Document.RevisionID || c.KnowledgeBaseID != doc.Document.KnowledgeBaseID || len(c.Text) > 16384 {
			return fmt.Errorf("invalid chunk metadata")
		}
		if err := validateVector(vectors[i], s.dimensions); err != nil {
			return err
		}
	}
	for start := 0; start < len(vectors); start += 64 {
		rows := make([]any, 0, min(64, len(vectors)-start))
		for i := start; i < min(start+64, len(vectors)); i++ {
			c := doc.Chunks[i]
			rows = append(rows, map[string]any{"id": c.ID, "vector": vectors[i], "kb": c.KnowledgeBaseID, "document_id": c.DocumentID, "revision_id": c.RevisionID, "source": c.Source, "text": c.Text, "chunk_index": c.Index})
		}
		if err := s.call(ctx, "entities/upsert", map[string]any{"data": rows}, nil); err != nil {
			return err
		}
	}
	filter := fmt.Sprintf("kb == %s and document_id == %s and revision_id != %s", strconv.Quote(doc.Document.KnowledgeBaseID), strconv.Quote(doc.Document.ID), strconv.Quote(doc.Document.RevisionID))
	return s.call(ctx, "entities/delete", map[string]any{"filter": filter}, nil)
}

// 模块 4：强一致查询，按知识库过滤。分数为 COSINE 相似度，不是正确率。
func (s *MilvusStore) Search(ctx context.Context, kb string, vector []float32, limit int) ([]Hit, error) {
	if !knowledgeBaseName.MatchString(kb) || limit < 1 || limit > 5 {
		return nil, fmt.Errorf("invalid search arguments")
	}
	if err := validateVector(vector, s.dimensions); err != nil {
		return nil, err
	}
	var hits []Hit
	err := s.call(ctx, "entities/search", map[string]any{"data": [][]float32{vector}, "annsField": "vector", "filter": "kb == " + strconv.Quote(kb), "limit": limit, "outputFields": []string{"source", "chunk_index", "text", "document_id", "revision_id"}, "consistencyLevel": "Strong", "searchParams": map[string]any{"metricType": "COSINE", "params": map[string]any{}}}, &hits)
	if err != nil {
		return nil, err
	}
	if len(hits) > limit {
		return nil, fmt.Errorf("Milvus result count exceeds limit")
	}
	for _, h := range hits {
		if math.IsNaN(h.Score) || math.IsInf(h.Score, 0) || len(h.Text) > 16384 {
			return nil, fmt.Errorf("invalid Milvus search result")
		}
	}
	return hits, nil
}

func (s *MilvusStore) Close() { s.http.CloseIdleConnections() }
