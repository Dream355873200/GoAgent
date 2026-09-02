package retrieval

import "context"

// Retriever 检索器——RAG 读路径的唯一窄接口。
// 实现方持有索引（向量库/BM25/外部 API），topK 由调用方决定。
// 返回的 Document 建议带 score 与 source（融合排序与引用审计的货币）。
type Retriever interface {
	Retrieve(ctx context.Context, query string, topK int) ([]Document, error)
}

// Embedder 向量化器——把文本变成向量（OpenAI/Qwen/Ollama 等都能实现）。
// 写入侧（Indexer 建索引）与查询侧（向量检索的 query 编码）共用。
type Embedder interface {
	EmbedStrings(ctx context.Context, texts []string) ([][]float32, error)
}

// Store 文档存储——RAG 写路径。把带 Embedding 的 Document 存入索引
// （向量库/pgvector/内存）。实现方决定去重与 upsert 语义。
type Store interface {
	Store(ctx context.Context, docs []Document) error
}

// Loader 文档加载器——从数据源读原始文档（文件/URL/数据库）。
// 与 Transformer（分块）解耦：加载只负责「读出来」，切块另算。
type Loader interface {
	Load(ctx context.Context, source string) ([]Document, error)
}

// Transformer 文档变换器——分块/清洗/ enrich。RAG 工程中效果差异
// 最大、最需独立实验的解耦点（ChunkTransformer 是参考实现）。
type Transformer interface {
	Transform(ctx context.Context, docs []Document) ([]Document, error)
}

// Reranker 重排器——可选精排：对初筛结果按与 query 的相关度重排。
// WithRetrieval 管道中 Retriever 与 prompt 拼装之间的可选一环。
type Reranker interface {
	Rerank(ctx context.Context, query string, docs []Document) ([]Document, error)
}
