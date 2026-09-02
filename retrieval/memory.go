package retrieval

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
)

// MemoryRetriever 关键词检索器（进程内，无外部依赖）。
// 打分：query 按空白切词，文档得分 = 各词在 Content 中的出现次数之和。
// 定位：测试/演示/小规模知识库兜底——生产 RAG 请接向量库或 BM25 引擎。
type MemoryRetriever struct {
	mu   sync.RWMutex
	docs []Document
}

// NewMemoryRetriever 构造并装载初始文档。
func NewMemoryRetriever(docs ...Document) *MemoryRetriever {
	return &MemoryRetriever{docs: append([]Document(nil), docs...)}
}

// Add 追加文档（并发安全）。
func (m *MemoryRetriever) Add(docs ...Document) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.docs = append(m.docs, docs...)
}

// Retrieve 实现 Retriever。
func (m *MemoryRetriever) Retrieve(_ context.Context, query string, topK int) ([]Document, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	terms := strings.Fields(query)
	out := make([]Document, 0, len(m.docs))
	for _, d := range m.docs {
		var score float64
		for _, term := range terms {
			score += float64(strings.Count(d.Content, term))
		}
		if score > 0 {
			out = append(out, d.WithScore(score))
		}
	}
	SortByScore(out)
	if topK > 0 && len(out) > topK {
		out = out[:topK]
	}
	return out, nil
}

// VectorStore 进程内余弦向量库（实现 Store + Retriever）。
// 写入侧：调方先用 Embedder 生成 Embedding 再 Store（或用 Index 辅助）。
// 查询侧：构造时传入 Embedder，Retrieve 自动编码 query 做余弦检索。
// 定位：零依赖的起步实现与测试夹具；大规模/持久化请接 pgvector 等。
type VectorStore struct {
	mu       sync.RWMutex
	docs     []Document
	embedder Embedder // 可为 nil（此时 Retrieve 返回空——只当 Store 用）
}

// NewVectorStore 构造向量库。embedder 可为 nil（仅存储，不可检索）。
func NewVectorStore(embedder Embedder) *VectorStore {
	return &VectorStore{embedder: embedder}
}

// Store 实现 Store：追加带 Embedding 的文档（无 Embedding 的跳过并
// 计数，返回提示——静默丢文档会让检索结果残缺却毫无征兆）。
func (v *VectorStore) Store(_ context.Context, docs []Document) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	skipped := 0
	for _, d := range docs {
		if len(d.Embedding) == 0 {
			skipped++
			continue
		}
		v.docs = append(v.docs, d)
	}
	if skipped > 0 {
		return fmt.Errorf("retrieval: VectorStore 跳过 %d 个无向量的文档（Store 前需先经 Embedder 生成 Embedding）", skipped)
	}
	return nil
}

// Retrieve 实现 Retriever：query 经 embedder 编码后按余弦相似度检索。
func (v *VectorStore) Retrieve(ctx context.Context, query string, topK int) ([]Document, error) {
	if v.embedder == nil {
		return nil, fmt.Errorf("retrieval: VectorStore 未配置 Embedder，无法编码查询")
	}
	vecs, err := v.embedder.EmbedStrings(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("retrieval: 查询向量化失败: %w", err)
	}
	if len(vecs) == 0 {
		return nil, nil
	}
	q := vecs[0]

	v.mu.RLock()
	defer v.mu.RUnlock()

	out := make([]Document, 0, len(v.docs))
	for _, d := range v.docs {
		if s := cosine(q, d.Embedding); !math.IsNaN(s) {
			out = append(out, d.WithScore(s))
		}
	}
	SortByScore(out)
	if topK > 0 && len(out) > topK {
		out = out[:topK]
	}
	return out, nil
}

// cosine 余弦相似度。维度不齐或零向量返回 NaN（调用方跳过）。
func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return math.NaN()
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return math.NaN()
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// Index 写入辅助：Loader → Transformer → Embedder → Store 一次串成
// （最常用的建索引流水线，各环节都可换成自己的实现）。
// 返回成功入库的文档数。
func Index(ctx context.Context, ldr Loader, tr Transformer, emb Embedder, st Store, source string) (int, error) {
	docs, err := ldr.Load(ctx, source)
	if err != nil {
		return 0, fmt.Errorf("retrieval: 加载失败: %w", err)
	}
	if tr != nil {
		docs, err = tr.Transform(ctx, docs)
		if err != nil {
			return 0, fmt.Errorf("retrieval: 变换失败: %w", err)
		}
	}
	if emb != nil {
		texts := make([]string, len(docs))
		for i, d := range docs {
			texts[i] = d.Content
		}
		vecs, err := emb.EmbedStrings(ctx, texts)
		if err != nil {
			return 0, fmt.Errorf("retrieval: 向量化失败: %w", err)
		}
		if len(vecs) != len(docs) {
			return 0, fmt.Errorf("retrieval: 向量数 %d 与文档数 %d 不符", len(vecs), len(docs))
		}
		for i := range docs {
			docs[i].Embedding = vecs[i]
		}
	}
	if err := st.Store(ctx, docs); err != nil {
		return 0, fmt.Errorf("retrieval: 入库失败: %w", err)
	}
	return len(docs), nil
}
