// retrieval 包底座测试：Document 元数据流转、MemoryRetriever 评分、
// VectorStore 余弦检索、ChunkTransformer 分块、FileLoader、FusionRetriever。
// 全部零外部依赖（Embedder 用确定性假实现）。
package retrieval_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dream355873200/GoAgent/retrieval"
)

// fakeEmbedder 确定性向量：按文本首字节给方向不同的单位向量，
// 相同首字母的文本相似度 1。够测余弦检索的排序语义。
type fakeEmbedder struct{}

func (fakeEmbedder) EmbedStrings(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, 8)
		if len(t) > 0 {
			v[int(t[0])%8] = 1
		} else {
			v[0] = 1
		}
		out[i] = v
	}
	return out, nil
}

func TestDocumentMetadataRoundtrip(t *testing.T) {
	d := retrieval.Document{Content: "hello", Metadata: map[string]any{"source": "a.md"}}
	d2 := d.WithScore(0.9)
	if d2.Source() != "a.md" {
		t.Errorf("WithScore 应保留既有元数据, source=%q", d2.Source())
	}
	if d2.Score() != 0.9 {
		t.Errorf("Score() = %v, want 0.9", d2.Score())
	}
	if d.Score() != 0 {
		t.Errorf("原值不应被改动（值语义）, Score() = %v", d.Score())
	}
	empty := retrieval.Document{}
	if empty.Score() != 0 || empty.Source() != "" {
		t.Errorf("空元数据应零值兜底")
	}
}

func TestMemoryRetrieverScoringAndTopK(t *testing.T) {
	r := retrieval.NewMemoryRetriever(
		retrieval.Document{Content: "go go go"},
		retrieval.Document{Content: "rust once"},
		retrieval.Document{Content: "go and rust"}, // 得分居中
	)
	docs, err := r.Retrieve(context.Background(), "go", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Fatalf("topK=2 应返回 2 条, got %d", len(docs))
	}
	if !strings.Contains(docs[0].Content, "go go go") {
		t.Errorf("最高分应是最多命中文档, got %q", docs[0].Content)
	}
	if docs[0].Score() <= docs[1].Score() {
		t.Errorf("应按分数降序: %v vs %v", docs[0].Score(), docs[1].Score())
	}

	// 无命中返回空（不报错）。
	none, _ := r.Retrieve(context.Background(), "python", 5)
	if len(none) != 0 {
		t.Errorf("无命中应返回空, got %d", len(none))
	}

	// 中文查询：MemoryRetriever 按空白切词，无空格中文整句是一个 term——
	// 这是已知局限（关键词检索器定位为测试/演示），不是 bug。分词需
	// 接真实检索引擎；这里验证「子串恰好命中」路径。
	cn := retrieval.NewMemoryRetriever(retrieval.Document{Content: "退款政策 七天无理由"})
	cnDocs, _ := cn.Retrieve(context.Background(), "退款政策", 5)
	if len(cnDocs) != 1 {
		t.Errorf("中文子串命中应返回 1 条, got %d", len(cnDocs))
	}
}

func TestVectorStoreRetrieval(t *testing.T) {
	vs := retrieval.NewVectorStore(fakeEmbedder{})
	ctx := context.Background()

	// 无向量文档应报错而非静默丢（错误里带跳过数）。
	err := vs.Store(ctx, []retrieval.Document{{Content: "no vec"}})
	if err == nil || !strings.Contains(err.Error(), "跳过") {
		t.Errorf("无向量文档应报错, got %v", err)
	}

	// 未配置 Embedder 的库不可检索（明确报错而非空结果）。
	_, err = retrieval.NewVectorStore(nil).Retrieve(ctx, "anything", 5)
	if err == nil || !strings.Contains(err.Error(), "Embedder") {
		t.Errorf("无 Embedder 检索应报错, got %v", err)
	}

	// 查询 "avocado"（首字节 'a' → 向量 [1,0,...]）应最近邻 apple。
	// Store 的契约是「存带向量的文档」——向量由调用方先经 Embedder 生成
	// （或用 Index 辅助流水线）。这里手动填，与 fakeEmbedder 的编码一致。
	vs2 := retrieval.NewVectorStore(fakeEmbedder{})
	vecs, err := fakeEmbedder{}.EmbedStrings(ctx, []string{"apple pie", "banana bread"})
	if err != nil {
		t.Fatal(err)
	}
	if err := vs2.Store(ctx, []retrieval.Document{
		{Content: "apple pie", Embedding: vecs[0]},
		{Content: "banana bread", Embedding: vecs[1]},
	}); err != nil {
		t.Fatal(err)
	}
	docs, err := vs2.Retrieve(ctx, "avocado", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) == 0 || !strings.Contains(docs[0].Content, "apple") {
		t.Errorf("余弦最近邻应是 apple, got %+v", docs)
	}
}

func TestChunkTransformer(t *testing.T) {
	tr := retrieval.ChunkTransformer{Size: 4, Overlap: 1}
	docs, err := tr.Transform(context.Background(), []retrieval.Document{
		{Content: "abcdefgh", Metadata: map[string]any{"source": "f.txt"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 8 字符 / (4-1 步长) → 3 块。
	if len(docs) != 3 {
		t.Fatalf("8 字符 size=4 overlap=1 应 3 块, got %d: %+v", len(docs), docs)
	}
	if docs[0].Content != "abcd" || docs[1].Content != "defg" || docs[2].Content != "gh" {
		t.Errorf("滑窗切块错误: %+v", docs)
	}
	if docs[0].Source() != "f.txt" {
		t.Errorf("块应继承 source: %+v", docs[0].Metadata)
	}
	if docs[0].Metadata["chunk_index"] != 0 || docs[0].Metadata["chunk_total"] != 3 {
		t.Errorf("块应带 chunk 定位: %+v", docs[0].Metadata)
	}

	// 短文档不分块原样透传。
	short, _ := tr.Transform(context.Background(), []retrieval.Document{{Content: "ab"}})
	if len(short) != 1 || short[0].Content != "ab" {
		t.Errorf("短文档应原样透传: %+v", short)
	}
}

func TestFileLoader(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("# hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "c.bin"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	var l retrieval.FileLoader
	docs, err := l.Load(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 { // .bin 跳过
		t.Fatalf("目录应加载 .md/.txt 两个, got %d", len(docs))
	}
	if docs[0].Source() == "" {
		t.Errorf("文档应带 source: %+v", docs[0].Metadata)
	}

	// 不存在路径：LLM 可读错误。
	if _, err := l.Load(context.Background(), filepath.Join(dir, "nope")); err == nil {
		t.Errorf("不存在路径应报错")
	}
}

func TestFusionRetriever(t *testing.T) {
	// 两路各给有序结果，融合应轮流取。
	r1 := retrieval.NewMemoryRetriever(retrieval.Document{Content: "a1"}, retrieval.Document{Content: "a2"})
	r2 := retrieval.NewMemoryRetriever(retrieval.Document{Content: "b1"}, retrieval.Document{Content: "b2"})

	f := &retrieval.FusionRetriever{Children: []retrieval.Retriever{r1, r2}}
	docs, err := f.Retrieve(context.Background(), "a1 a2 b1 b2", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 3 {
		t.Fatalf("topK=3 应返回 3, got %d", len(docs))
	}
	// interleave：第一路第 1 条、第二路第 1 条、第一路第 2 条。
	if docs[0].Content != "a1" || docs[1].Content != "b1" {
		t.Errorf("融合应路间轮流: %+v", docs)
	}

	// 子检索器报错 → 整体报错（fail-closed，不静默丢一路）。
	boom := errorRetriever{}
	f2 := &retrieval.FusionRetriever{Children: []retrieval.Retriever{r1, boom}}
	if _, err := f2.Retrieve(context.Background(), "x", 5); err == nil {
		t.Errorf("子检索器失败应整体报错")
	}

	// 无子检索器：构造错误。
	if _, err := (&retrieval.FusionRetriever{}).Retrieve(context.Background(), "x", 5); err == nil {
		t.Errorf("空 Children 应报错")
	}
}

type errorRetriever struct{}

func (errorRetriever) Retrieve(context.Context, string, int) ([]retrieval.Document, error) {
	return nil, errors.New("boom")
}

// Index 辅助流水线：Loader → Transformer → Embedder → Store 全链。
func TestIndexPipeline(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "doc.md")
	content := strings.Repeat("abcdefgh", 20) // 160 字符 → size=32 时 6 块
	if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	vs := retrieval.NewVectorStore(fakeEmbedder{})
	n, err := retrieval.Index(context.Background(),
		retrieval.FileLoader{}, retrieval.ChunkTransformer{Size: 32, Overlap: 4},
		fakeEmbedder{}, vs, dir)
	if err != nil {
		t.Fatal(err)
	}
	if n != 6 {
		t.Fatalf("160 字符 size=32 overlap=4 应 6 块, got %d", n)
	}

	docs, err := vs.Retrieve(context.Background(), "apple", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 3 {
		t.Fatalf("检索应命中 3 块, got %d", len(docs))
	}
	// 检索命中的应是分块（带 chunk_index），source 指向原文件。
	for _, d := range docs {
		if d.Source() != src {
			t.Errorf("块的 source 应指原文件 %s, got %q", src, d.Source())
		}
	}
}
