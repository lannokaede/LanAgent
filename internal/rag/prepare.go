// Package rag 负责知识库的数据准备、索引和检索；当前实现文本读取与分块。
package rag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/tmc/langchaingo/textsplitter"
)

const (
	MaxDocumentBytes = 4 << 20
	MaxChunkRunes    = 4096
	splitterVersion  = "langchaingo-v0.1.15-recursive-character-v1"
)

var knowledgeBaseName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// 模块 1：数据契约。分块必须保留知识库、文档、版本和来源，方便后续
// 向量写入/删除及答案引用。当前只以源文件名和块序号定位，不伪造页码。
type SplitConfig struct {
	ChunkSize    int `json:"chunk_size"`
	ChunkOverlap int `json:"chunk_overlap"`
}

type Document struct {
	ID              string `json:"id"`
	KnowledgeBaseID string `json:"knowledge_base_id"`
	Filename        string `json:"filename"`
	ContentSHA256   string `json:"content_sha256"`
	RevisionID      string `json:"revision_id"`
}

type Chunk struct {
	ID              string `json:"id"`
	KnowledgeBaseID string `json:"knowledge_base_id"`
	DocumentID      string `json:"document_id"`
	RevisionID      string `json:"revision_id"`
	Index           int    `json:"index"`
	Source          string `json:"source"`
	Text            string `json:"text"`
}

type PreparedDocument struct {
	Document   Document    `json:"document"`
	Status     string      `json:"status"`
	Splitter   string      `json:"splitter"`
	Config     SplitConfig `json:"config"`
	LengthUnit string      `json:"length_unit"`
	Chunks     []Chunk     `json:"chunks"`
}

// 模块 2：分块配置。长度单位是 Unicode 字符（rune），不是模型 Token。
// 字符预算只能用于预处理；接 Embedding 时还必须按实际模型 Token 上限校验。
// TODO：引入对应 Embedding tokenizer，并在索引阶段记录模型/维度/实现版本。
type Preparer struct {
	config   SplitConfig
	splitter textsplitter.RecursiveCharacter
}

func NewPreparer(config SplitConfig) (*Preparer, error) {
	if config.ChunkSize <= 0 || config.ChunkSize > MaxChunkRunes {
		return nil, fmt.Errorf("chunk size must be between 1 and %d", MaxChunkRunes)
	}
	if config.ChunkOverlap < 0 || config.ChunkOverlap >= config.ChunkSize {
		return nil, fmt.Errorf("chunk overlap must be non-negative and smaller than chunk size")
	}
	return &Preparer{config: config, splitter: textsplitter.NewRecursiveCharacter(
		textsplitter.WithChunkSize(config.ChunkSize), textsplitter.WithChunkOverlap(config.ChunkOverlap),
		textsplitter.WithLenFunc(utf8.RuneCountInString),
		textsplitter.WithSeparators([]string{"\n\n", "\n", "。", "！", "？", " ", ""}),
		textsplitter.WithKeepSeparator(true),
	)}, nil
}

// 模块 3：读取并校验。允许 UTF-8 TXT/Markdown，限制体积，统一 BOM/换行。
// 传入 Reader 使 CLI 和未来 HTTP 可以共用同一处理流程；这里不执行文件内容。
func (p *Preparer) Prepare(ctx context.Context, kb, filename string, reader io.Reader) (*PreparedDocument, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !knowledgeBaseName.MatchString(kb) {
		return nil, fmt.Errorf("invalid knowledge base ID")
	}
	// 只保存显示名称，不把调用者的完整本机路径写入来源元数据。
	filename = path.Base(strings.ReplaceAll(filename, "\\", "/"))
	if len(filename) > 255 || strings.ContainsAny(filename, "\x00\r\n") {
		return nil, fmt.Errorf("invalid filename")
	}
	switch strings.ToLower(path.Ext(filename)) {
	case ".txt", ".md", ".markdown":
	default:
		return nil, fmt.Errorf("only UTF-8 .txt, .md and .markdown documents are supported")
	}
	if reader == nil {
		return nil, fmt.Errorf("document reader is nil")
	}
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx: ctx, reader: reader}, MaxDocumentBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read document: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(data) > MaxDocumentBytes {
		return nil, fmt.Errorf("document exceeds %d byte limit", MaxDocumentBytes)
	}
	if !utf8.Valid(data) || strings.ContainsRune(string(data), 0) {
		return nil, fmt.Errorf("document must contain valid UTF-8 text without NUL bytes")
	}
	text := strings.TrimPrefix(string(data), "\ufeff")
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("document is empty")
	}

	// 模块 4：框架分块与可复现 ID。同知识库同文件名代表同一文档；内容或
	// 分块配置变化代表新版本。后续索引更新可据此替换旧版本，避免重复写入。
	parts, err := p.splitter.SplitText(text)
	if err != nil {
		return nil, fmt.Errorf("split document: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	contentHash := digest(text)
	doc := Document{ID: digest(kb + "\x00" + filename), KnowledgeBaseID: kb, Filename: filename, ContentSHA256: contentHash,
		RevisionID: digest(fmt.Sprintf("%s:%s:%d:%d", splitterVersion, contentHash, p.config.ChunkSize, p.config.ChunkOverlap))}
	result := &PreparedDocument{Document: doc, Status: "prepared", Splitter: splitterVersion, Config: p.config, LengthUnit: "rune", Chunks: make([]Chunk, 0, len(parts))}
	for _, part := range parts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if strings.TrimSpace(part) == "" {
			continue
		}
		if utf8.RuneCountInString(part) > p.config.ChunkSize {
			return nil, fmt.Errorf("splitter produced an oversized chunk")
		}
		index := len(result.Chunks)
		result.Chunks = append(result.Chunks, Chunk{ID: digest(fmt.Sprintf("%s:%s:%d", doc.ID, doc.RevisionID, index)), KnowledgeBaseID: kb,
			DocumentID: doc.ID, RevisionID: doc.RevisionID, Index: index, Source: filename, Text: part})
	}
	if len(result.Chunks) == 0 {
		return nil, fmt.Errorf("document produced no usable chunks")
	}
	// prepared 只表示预处理完成，不表示已生成向量或可在知识库检索。
	return result, nil
}

func digest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// 在每次读取前检查取消。任意 Reader 正在阻塞时仍需由其自身实现超时/关闭；
// 当前 CLI 使用本地普通文件，未来 HTTP 使用带请求限制的 Body。
type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
