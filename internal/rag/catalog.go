package rag

import (
	"context"
	"fmt"
	"sort"
	"strconv"
)

// 已入库目录来自 Milvus，而非浏览器缓存；因此能显示之前 CLI 导入的文档。
// Demo 目录扫描上限 10000 片段，超限明确报错；TODO：大库使用独立文档清单。
func (s *MilvusStore) ListDocuments(ctx context.Context, kb string) ([]DocumentInfo, error) {
	if !knowledgeBaseName.MatchString(kb) {
		return nil, ErrKnowledgeInput
	}
	var rows []struct {
		DocumentID string `json:"document_id"`
		RevisionID string `json:"revision_id"`
		Source     string `json:"source"`
	}
	err := s.call(ctx, "entities/query", map[string]any{"filter": "kb == " + strconv.Quote(kb), "limit": 10001, "outputFields": []string{"document_id", "revision_id", "source"}, "consistencyLevel": "Strong"}, &rows)
	if err != nil {
		return nil, err
	}
	if len(rows) > 10000 {
		return nil, fmt.Errorf("knowledge catalog exceeds 10000 chunks; a paginated manifest is required")
	}
	docs := make(map[string]DocumentInfo)
	for _, row := range rows {
		if !documentIDPattern.MatchString(row.DocumentID) {
			return nil, fmt.Errorf("invalid document metadata")
		}
		info, exists := docs[row.DocumentID]
		if !exists {
			info = DocumentInfo{ID: row.DocumentID, Filename: row.Source, RevisionID: row.RevisionID, Status: "indexed"}
		}
		if info.RevisionID != row.RevisionID {
			info.Status = "needs_retry"
			info.Error = "存在多个文档版本，请重新上传完成版本清理。"
		}
		info.ChunkCount++
		docs[row.DocumentID] = info
	}
	result := make([]DocumentInfo, 0, len(docs))
	for _, doc := range docs {
		result = append(result, doc)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}
func (s *MilvusStore) DeleteDocument(ctx context.Context, kb, id string) error {
	if !knowledgeBaseName.MatchString(kb) || !documentIDPattern.MatchString(id) {
		return ErrKnowledgeInput
	}
	return s.call(ctx, "entities/delete", map[string]any{"filter": "kb == " + strconv.Quote(kb) + " and document_id == " + strconv.Quote(id)}, nil)
}
