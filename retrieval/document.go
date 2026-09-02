// Package retrieval 实现 RAG（检索增强生成）的共享底座与双形态接线。
//
// 形态定位（README 补缺清单定稿）——RAG 在应用里是嵌入管道为主、
// agent 工具为辅：
//
//	共享底座：Document 类型（Content+Metadata 贯穿全程，审计引用来源）
//	         + 窄接口四兄弟（Retriever/Embedder/Store/Loader——对齐
//	           eino 接口密度，每个一两个方法，没人会实现错）
//	    ├─ 形态 1（主）：WithRetrieval 嵌入管道——App.run 前置固定流程，
//	    │   retrieve→rerank→拼 prompt，agent 无感知。可靠性/成本可控/
//	    │   可独立 A/B 调优——客服/文档问答等生产 RAG 的主流形态
//	    │   （含 ShouldRetrieve 谓词：闲聊跳过检索省成本）
//	    └─ 形态 2（辅）：NewRetrieveTool 注册为工具——agent 运行时自己
//	        决定查不查/查几轮/换什么词再查。研究型任务、多跳问题。
//
// 设计约定：
//   - 组合模式：多路检索（向量+BM25+图谱融合）走 FusionRetriever
//     （Children + Merge 策略，使用者普通 Go 代码）——框架不穷举组合，
//     拓扑自由度从框架转移给使用者
//   - 分块策略独立：Loader/Transformer 解耦——RAG 工程中效果差异最大、
//     最需独立实验的解耦点
//   - 边界声明（诚实）：WithRetrieval 单链覆盖单路前置检索；分叉/多路
//     走组合模式与谓词；迭代检索/中段检索走工具形态或选用 graph 系
//     框架。运行时动态改检索管道（热切换）= 基础设施职责
package retrieval

import (
	"sort"
)

// Document 是检索全程的统一数据单元：加载→分块→嵌入→存储→检索→注入。
// Metadata 贯穿全程（source/chunk_index 等），是引用审计的来源。
type Document struct {
	// Content 文本内容。
	Content string

	// Metadata 任意元数据：source（来源路径/URL）、chunk_index、
	// title 等。检索侧可写入 score（float64）供排序/融合。
	Metadata map[string]any

	// Embedding 可选：向量存储的写入方负责填充（如 Indexer 用
	// Embedder 生成后回填）。纯关键词检索用不到。
	Embedding []float32
}

// WithMetadata 返回追加了元数据的新 Document（不改动原值）。
func (d Document) WithMetadata(key string, value any) Document {
	m := make(map[string]any, len(d.Metadata)+1)
	for k, v := range d.Metadata {
		m[k] = v
	}
	m[key] = value
	d.Metadata = m
	return d
}

// Score 读取 Metadata["score"]（检索器/融合器写入）。无分数返回 0。
func (d Document) Score() float64 {
	if v, ok := d.Metadata["score"]; ok {
		if f, ok := v.(float64); ok {
			return f
		}
	}
	return 0
}

// WithScore 返回带分数的新 Document（排序/融合的通用货币）。
func (d Document) WithScore(score float64) Document {
	return d.WithMetadata("score", score)
}

// Source 读取 Metadata["source"]（引用审计用）。无来源返回空串。
func (d Document) Source() string {
	if v, ok := d.Metadata["source"]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// SortByScore 按分数降序原地排序（无分数的排后面）。
func SortByScore(docs []Document) {
	sort.SliceStable(docs, func(i, j int) bool { return docs[i].Score() > docs[j].Score() })
}
