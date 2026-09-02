// retrieval.go RAG 双形态接线（形态 1 嵌入管道 + 形态 2 agent 工具）。
// 共享底座（Document/窄接口/Fusion/参考实现）在 retrieval 子包，
// 这里只做 App 级装配与类型别名（用户实现接口时不必再引子包路径）。
package goagent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Dream355873200/GoAgent/retrieval"
)

// 类型别名：常用契约直接从根包引用（与 Sandbox 等子系统同 ergonomic）。
type (
	// Document 检索全程的统一数据单元（Content+Metadata+Embedding）。
	Document = retrieval.Document
	// Retriever 检索器窄接口（RAG 读路径）。
	Retriever = retrieval.Retriever
	// Embedder 向量化器窄接口。
	Embedder = retrieval.Embedder
	// Store 文档存储窄接口（RAG 写路径）。
	Store = retrieval.Store
	// Loader 文档加载器窄接口（与 Transformer 分块解耦）。
	Loader = retrieval.Loader
	// Transformer 文档变换器窄接口（分块/清洗）。
	Transformer = retrieval.Transformer
	// Reranker 重排器窄接口（可选精排）。
	Reranker = retrieval.Reranker
)

// RetrievalConfig 形态 1（嵌入管道）的配置。
// App.run 前置固定流程：ShouldRetrieve → retrieve → rerank → 拼进用户
// 输入，agent 无感知。适合客服/文档问答等生产 RAG——可靠性/成本可控，
// 可独立 A/B 调优。
type RetrievalConfig struct {
	// Retriever 必填。单路检索；多路融合用 retrieval.FusionRetriever。
	Retriever Retriever

	// Reranker 可选精排（Retrieve 之后、拼装之前）。
	Reranker Reranker

	// TopK 检索条数，<=0 时取 5。
	TopK int

	// MaxChars 注入内容的字符上限（防单次检索撑爆上下文），
	// <=0 不限制。截断时保序（分数高的先进）并注明截断。
	MaxChars int

	// ShouldRetrieve 谓词：返回 false 跳过检索（闲聊省成本）。
	// nil = 总是检索。
	ShouldRetrieve func(ctx context.Context, query string) bool

	// Timeout 检索（含重排）的总超时；0 = 不额外限时（继承调用 ctx）。
	Timeout time.Duration
}

// WithRetrieval 启用形态 1：RAG 嵌入管道（每次 run 前置检索）。
//
// 检索失败按错误处理（fail-closed）：向量库挂了还照常回答会产生
// 「看起来正常实则无据」的回答，宁可让上层显式处理；闲聊等场景
// 用 ShouldRetrieve 谓词短路。
//
// 边界（诚实声明）：单链覆盖单路前置检索；分叉/多路走 FusionRetriever
// 组合与谓词；迭代检索/中段检索用 NewRetrieveTool（形态 2）。
func WithRetrieval(cfg RetrievalConfig) Option {
	return optionFunc(func(c *appConfig) {
		c.retrieval = &cfg
	})
}

// NewRetrieveTool 形态 2：把检索注册为 agent 运行时可自主调用的工具。
// agent 自己决定查不查/查几轮/换什么词再查——研究型任务、多跳问题、
// 检索是与代码/文件工具同级的现场决策时用这个形态。
//
//	app.UseTools(goagent.NewRetrieveTool(myRetriever))
//
// topK 参数可被工具调用的 top_k 入参覆盖（默认 5）。
func NewRetrieveTool(r Retriever) NamedTool {
	type retrieveInput struct {
		Query string `json:"query" desc:"检索查询词；换个说法多查几轮是被鼓励的" required:"true"`
		TopK  int    `json:"top_k,omitempty" desc:"返回条数，默认 5"`
	}
	return InferTool("retrieve", "检索知识库，返回与查询相关的文档片段（含来源）。",
		func(ctx context.Context, in retrieveInput) (string, error) {
			topK := in.TopK
			if topK <= 0 {
				topK = 5
			}
			if topK > 50 {
				topK = 50 // 上限护栏：防止误传大数撑爆上下文
			}
			docs, err := r.Retrieve(ctx, in.Query, topK)
			if err != nil {
				return "", fmt.Errorf("检索失败: %w", err)
			}
			if len(docs) == 0 {
				return "（知识库中无相关文档）", nil
			}
			return FormatDocuments(docs), nil
		}, WithConcurrent(), // 只读检索，可并发
	)
}

// injectRetrieval 形态 1 的 run 前置流程：检索→重排→格式化→拼进输入。
// 返回增强后的 input；检索命中 0 条时原样返回（不注入空块污染对话）。
// 发送一条 EventRetrieval 事件（宿主可观测检索开销）。
func (a *App) injectRetrieval(ctx context.Context, cfg *RetrievalConfig, input string, out chan<- Event) (string, error) {
	if cfg.Retriever == nil {
		return input, nil
	}
	if cfg.ShouldRetrieve != nil && !cfg.ShouldRetrieve(ctx, input) {
		return input, nil
	}

	if cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
	}

	topK := cfg.TopK
	if topK <= 0 {
		topK = 5
	}
	docs, err := cfg.Retriever.Retrieve(ctx, input, topK)
	if err != nil {
		return "", fmt.Errorf("goagent: 检索失败: %w", err)
	}
	if cfg.Reranker != nil && len(docs) > 0 {
		docs, err = cfg.Reranker.Rerank(ctx, input, docs)
		if err != nil {
			return "", fmt.Errorf("goagent: 重排失败: %w", err)
		}
	}
	if len(docs) == 0 {
		return input, nil
	}

	block := FormatDocuments(docs)
	if cfg.MaxChars > 0 && len(block) > cfg.MaxChars {
		// 保序贪心：分数高的（排序在前）优先整条保留；预算装不下的
		// 整条省略并计数。第一条超预算时硬截（保序优先于完整性——
		// 最高分内容哪怕截断也要在场，不能被整条丢掉）。
		truncated := 0
		var kept []Document
		total := 0
		for i, d := range docs {
			if i > 0 && total+len(d.Content) > cfg.MaxChars {
				truncated++
				continue
			}
			if i > 0 {
				total += len(d.Content)
			}
			kept = append(kept, d)
		}
		notes := ""
		if truncated > 0 {
			notes = fmt.Sprintf("\n（另有 %d 条因超出长度上限被省略）", truncated)
		}
		if block2 := FormatDocuments(kept); len(block2) <= cfg.MaxChars {
			block = block2 + notes
		} else {
			// 头一条超预算：硬截。截断点至少伸到第一条内容内部 1/4
			// 预算（MaxChars 小到装不下 header 时，不能只给模型留一个
			// 空壳引用块）——下限 = 内容起点 + MaxChars/4，与 MaxChars
			// 取较大者。
			contentStart := len(block2) - totalAllContent(kept) // 近似：头部偏移
			cut := cfg.MaxChars
			if lower := contentStart + cfg.MaxChars/4; lower > cut {
				cut = lower
			}
			if cut > len(block2) {
				cut = len(block2)
			}
			block = block2[:cut] + "\n…（已截断）" + notes
		}
	}

	out <- Event{
		Type: EventRetrieval,
		Text: fmt.Sprintf("query=%.60q docs=%d chars=%d", input, len(docs), len(block)),
	}
	return block + "\n\n" + input, nil
}

// totalAllContent 求 kept 集合的内容总长（计算截断头部偏移用）。
func totalAllContent(docs []Document) int {
	n := 0
	for _, d := range docs {
		n += len(d.Content)
	}
	return n
}

// FormatDocuments 把检索结果格式化为可注入 prompt 的引用块。
// 每个 document 保留 source 元数据（引用审计），score 进注释。
func FormatDocuments(docs []Document) string {
	var b strings.Builder
	b.WriteString("<retrieved-context>\n")
	b.WriteString("以下内容由检索系统自动提供，供回答参考；引用时注明来源。\n")
	for i, d := range docs {
		src := d.Source()
		header := fmt.Sprintf("<document index=\"%d\"", i+1)
		if src != "" {
			header += fmt.Sprintf(" source=%q", src)
		}
		if s := d.Score(); s > 0 {
			header += fmt.Sprintf(" score=\"%.4f\"", s)
		}
		header += ">\n"
		b.WriteString(header)
		b.WriteString(d.Content)
		b.WriteString("\n</document>\n")
	}
	b.WriteString("</retrieved-context>")
	return b.String()
}
