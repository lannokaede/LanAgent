package rag

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"
)

var (
	ErrKnowledgeBusy     = errors.New("知识库正在导入或删除，请稍后重试")
	ErrKnowledgeNotFound = errors.New("文档不存在")
	ErrKnowledgeCapacity = errors.New("导入队列已满，请等待任务完成")
	ErrKnowledgeRecords  = errors.New("任务记录已达 100 条，请删除旧任务记录后重试")
	ErrKnowledgeInput    = errors.New("文档或检索参数无效")
)
var documentIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type DocumentInfo struct {
	ID         string `json:"id"`
	Filename   string `json:"filename"`
	RevisionID string `json:"revision_id"`
	Status     string `json:"status"`
	ChunkCount int    `json:"chunk_count"`
	UpdatedAt  string `json:"updated_at,omitempty"`
	Error      string `json:"error,omitempty"`
}
type DocumentCatalog interface {
	ListDocuments(context.Context, string) ([]DocumentInfo, error)
	DeleteDocument(context.Context, string, string) error
}
type KnowledgeIndexer interface {
	Index(context.Context, *PreparedDocument) error
	Search(context.Context, string, string, int) ([]Hit, error)
}

// 模块 1：HTTP 后台任务边界。导入脱离单次 HTTP 连接，跟随服务生命周期。
// 队列最多 4 个任务，所有写操作串行；同文档运行期间不能更新或删除。
// 向量/已入库目录在 Milvus，任务状态在内存。重启后未完成任务需重新上传。
type Manager struct {
	ctx       context.Context
	kb        string
	indexer   KnowledgeIndexer
	catalog   DocumentCatalog
	mu        sync.RWMutex
	jobs      map[string]DocumentInfo
	slots     chan struct{}
	writeGate chan struct{}
}

func NewManager(ctx context.Context, kb string, indexer KnowledgeIndexer, catalog DocumentCatalog) *Manager {
	return &Manager{ctx: ctx, kb: kb, indexer: indexer, catalog: catalog, jobs: make(map[string]DocumentInfo), slots: make(chan struct{}, 4), writeGate: make(chan struct{}, 1)}
}
func (m *Manager) KnowledgeBase() string { return m.kb }
func active(status string) bool {
	return status == "queued" || status == "indexing" || status == "deleting"
}
func (m *Manager) Submit(doc *PreparedDocument) (DocumentInfo, error) {
	if doc == nil || doc.Document.KnowledgeBaseID != m.kb || !documentIDPattern.MatchString(doc.Document.ID) || len(doc.Chunks) == 0 || len(doc.Chunks) > 2048 {
		return DocumentInfo{}, ErrKnowledgeInput
	}
	if err := m.ctx.Err(); err != nil {
		return DocumentInfo{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if previous, exists := m.jobs[doc.Document.ID]; exists && active(previous.Status) {
		return DocumentInfo{}, ErrKnowledgeBusy
	}
	// 防止失败任务记录无限增长；已入库历史不加载到 jobs。
	if _, exists := m.jobs[doc.Document.ID]; !exists && len(m.jobs) >= 100 {
		return DocumentInfo{}, ErrKnowledgeRecords
	}
	select {
	case m.slots <- struct{}{}:
	default:
		return DocumentInfo{}, ErrKnowledgeCapacity
	}
	info := DocumentInfo{ID: doc.Document.ID, Filename: doc.Document.Filename, RevisionID: doc.Document.RevisionID, Status: "queued", UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
	m.jobs[info.ID] = info
	go m.importDocument(doc)
	return info, nil
}
func (m *Manager) importDocument(doc *PreparedDocument) {
	defer func() { <-m.slots }()
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Minute)
	defer cancel()
	select {
	case m.writeGate <- struct{}{}:
	case <-ctx.Done():
		m.finish(doc, ctx.Err())
		return
	}
	m.mu.Lock()
	info := m.jobs[doc.Document.ID]
	info.Status = "indexing"
	m.jobs[info.ID] = info
	m.mu.Unlock()
	err := m.indexer.Index(ctx, doc)
	<-m.writeGate
	m.finish(doc, err)
}
func (m *Manager) finish(doc *PreparedDocument, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	info := m.jobs[doc.Document.ID]
	info.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err != nil {
		info.Status = "failed"
		info.Error = "导入失败或已取消，请检查模型与向量库连接后重新上传。部分片段可能已写入，重试可修复。"
	} else {
		info.Status = "indexed"
		info.ChunkCount = len(doc.Chunks)
		info.Error = ""
	}
	m.jobs[info.ID] = info
}

// 模块 2：持久化目录与当前任务合并。CLI 导入的文档也会在页面中显示。
func (m *Manager) List(ctx context.Context) ([]DocumentInfo, error) {
	stored, err := m.catalog.ListDocuments(ctx, m.kb)
	if err != nil {
		return nil, err
	}
	merged := make(map[string]DocumentInfo, len(stored))
	for _, doc := range stored {
		merged[doc.ID] = doc
	}
	m.mu.RLock()
	for id, info := range m.jobs {
		// CLI 可能已导入更新版本，完成状态不能覆盖向量库中的新版本元数据。
		if stored, exists := merged[id]; exists && info.Status == "indexed" && stored.RevisionID != info.RevisionID {
			continue
		}
		merged[id] = info
	}
	m.mu.RUnlock()
	result := make([]DocumentInfo, 0, len(merged))
	for _, info := range merged {
		result = append(result, info)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Filename == result[j].Filename {
			return result[i].ID < result[j].ID
		}
		return result[i].Filename < result[j].Filename
	})
	return result, nil
}
func (m *Manager) Get(ctx context.Context, id string) (DocumentInfo, error) {
	if !documentIDPattern.MatchString(id) {
		return DocumentInfo{}, ErrKnowledgeNotFound
	}
	m.mu.RLock()
	info, exists := m.jobs[id]
	m.mu.RUnlock()
	if exists && info.Status != "indexed" {
		return info, nil
	}
	docs, err := m.List(ctx)
	if err != nil {
		return DocumentInfo{}, err
	}
	for _, info := range docs {
		if info.ID == id {
			return info, nil
		}
	}
	return DocumentInfo{}, ErrKnowledgeNotFound
}

// 模块 3：删除限定到绑定的知识库和文档 ID，包含所有版本。正在写库时返回冲突。
func (m *Manager) Delete(ctx context.Context, id string) error {
	if !documentIDPattern.MatchString(id) {
		return ErrKnowledgeNotFound
	}
	select {
	case m.writeGate <- struct{}{}:
		defer func() { <-m.writeGate }()
	default:
		return ErrKnowledgeBusy
	}
	m.mu.Lock()
	previous, exists := m.jobs[id]
	if exists && active(previous.Status) {
		m.mu.Unlock()
		return ErrKnowledgeBusy
	}
	m.jobs[id] = DocumentInfo{ID: id, Filename: previous.Filename, Status: "deleting"}
	m.mu.Unlock()
	err := m.catalog.DeleteDocument(ctx, m.kb, id)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		if exists {
			m.jobs[id] = previous
		} else {
			delete(m.jobs, id)
		}
		return err
	}
	delete(m.jobs, id)
	return nil
}
func (m *Manager) Search(ctx context.Context, query string, limit int) ([]Hit, error) {
	if err := validateSearch(m.kb, query, limit); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrKnowledgeInput, err)
	}
	return m.indexer.Search(ctx, m.kb, query, limit)
}
