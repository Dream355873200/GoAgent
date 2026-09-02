package retrieval

import (
	"context"
	"fmt"
	"sync"
)

// FusionRetriever 多路检索融合器（组合模式为设计约定）：
// Children 并发各自检索，Merge 策略把多路结果合成一路。
// 多路（向量+BM25+图谱）在这里用普通 Go 代码组合——框架不穷举组合，
// 拓扑自由度从框架转移给使用者（类型检查/可调试/IDE 全在）。
//
// 任一子检索器报错则整体报错（fail-closed）：静默丢一路会导致检索
// 结果看起来正常实则缺料，宁可让上层显式处理。
type FusionRetriever struct {
	// Children 参与融合的子检索器（至少一个）。
	Children []Retriever

	// Merge 融合策略：入参是各子检索器的结果（与 Children 同序），
	// 出参是融合后的最终列表（长度自定，建议不超过 topK）。
	// 预置策略见 MergeInterleaveByScore；nil 时用它兜底。
	Merge func(query string, results [][]Document, topK int) []Document
}

// Retrieve 实现 Retriever。子检索器并发执行（ctx 取消时全部中止）。
func (f *FusionRetriever) Retrieve(ctx context.Context, query string, topK int) ([]Document, error) {
	if len(f.Children) == 0 {
		return nil, fmt.Errorf("retrieval: FusionRetriever 无子检索器")
	}

	results := make([][]Document, len(f.Children))
	errs := make([]error, len(f.Children))
	var wg sync.WaitGroup
	for i, child := range f.Children {
		wg.Add(1)
		go func(i int, child Retriever) {
			defer wg.Done()
			docs, err := child.Retrieve(ctx, query, topK)
			results[i] = docs
			errs[i] = err
		}(i, child)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("retrieval: 子检索器 %d 失败: %w", i, err)
		}
	}

	merge := f.Merge
	if merge == nil {
		merge = MergeInterleaveByScore
	}
	return merge(query, results, topK), nil
}

// MergeInterleaveByScore 预置融合策略：每路内部按分数降序，路间轮流
// 取一（interleave）——不信任任何单路的绝对分数（各索引分数量纲不同），
// 只信任路内相对序。最终截断 topK。
func MergeInterleaveByScore(_ string, results [][]Document, topK int) []Document {
	for _, docs := range results {
		SortByScore(docs)
	}
	var merged []Document
	for round := 0; ; round++ {
		took := false
		for _, docs := range results {
			if round < len(docs) {
				merged = append(merged, docs[round])
				took = true
				if topK > 0 && len(merged) >= topK {
					return merged[:topK]
				}
			}
		}
		if !took {
			return merged
		}
	}
}
