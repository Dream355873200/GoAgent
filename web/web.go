// Package web 提供 WebFetch 和 WebSearch 工具实现。
//
// 对齐 Claude Code 的 src/tools/WebFetchTool/ 和 src/tools/WebSearchTool/：
//   - WebFetch: HTTP GET 抓取网页，HTML→纯文本转换，内容截断
//   - WebSearch: 接入可配置的搜索后端（Brave/Tavily/SerpAPI/自定义）
//
// 使用方式：通过 goagent.UseTools() 注册。
package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/anthropic-community/goagent"
)

// ── WebFetch ──────────────────────────────────────────────────

// WebFetchInput 是 WebFetch 工具的输入。
type WebFetchInput struct {
	// URL 是要抓取的网页地址。
	URL string `json:"url" description:"要抓取的网页 URL"`
	// Prompt 是可选的提取提示（目前未使用，预留给未来 LLM 提取）。
	Prompt string `json:"prompt,omitempty" description:"可选：对网页内容的提取提示"`
}

// WebFetchConfig 配置 WebFetch 行为。
type WebFetchConfig struct {
	// MaxBodySize 是最大响应体字节数。默认 512KB。
	MaxBodySize int64
	// Timeout 是 HTTP 超时时间。默认 30 秒。
	Timeout time.Duration
	// UserAgent 是自定义 User-Agent。
	UserAgent string
}

// DefaultFetchConfig 返回默认配置。
func DefaultFetchConfig() WebFetchConfig {
	return WebFetchConfig{
		MaxBodySize: 512 * 1024,
		Timeout:     30 * time.Second,
		UserAgent:   "GoAgent/1.0 (WebFetch)",
	}
}

// WebFetchTool 返回 WebFetch 工具定义。
func WebFetchTool(cfg ...WebFetchConfig) goagent.ToolDef {
	c := DefaultFetchConfig()
	if len(cfg) > 0 {
		c = cfg[0]
	}

	return goagent.ToolDef{
		Description: "抓取指定 URL 的网页内容并返回纯文本。支持 HTTP/HTTPS。",
		Input:       WebFetchInput{},
		Execute: func(ctx goagent.Context, in WebFetchInput) (string, error) {
			return executeFetch(ctx, in, c)
		},
	}
}

func executeFetch(ctx goagent.Context, in WebFetchInput, cfg WebFetchConfig) (string, error) {
	if in.URL == "" {
		return "", fmt.Errorf("URL 不能为空")
	}

	// 自动补 https。
	if !strings.HasPrefix(in.URL, "http://") && !strings.HasPrefix(in.URL, "https://") {
		in.URL = "https://" + in.URL
	}

	client := &http.Client{Timeout: cfg.Timeout}
	req, err := http.NewRequestWithContext(ctx, "GET", in.URL, nil)
	if err != nil {
		return "", fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("User-Agent", cfg.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain,*/*")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	// 读取受限的响应体。
	body, err := io.ReadAll(io.LimitReader(resp.Body, cfg.MaxBodySize))
	if err != nil {
		return "", fmt.Errorf("读取响应失败: %w", err)
	}

	content := string(body)

	// 简单的 HTML → 纯文本转换。
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "html") {
		content = htmlToText(content)
	}

	// 截断过长内容。
	const maxChars = 100_000
	if len(content) > maxChars {
		content = content[:maxChars] + "\n... (内容已截断)"
	}

	return content, nil
}

// htmlToText 简单地将 HTML 转换为纯文本。
func htmlToText(html string) string {
	// 移除 script 和 style 标签及其内容。
	reScript := regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	html = reScript.ReplaceAllString(html, "")
	reStyle := regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	html = reStyle.ReplaceAllString(html, "")

	// 换行标签转换。
	reBr := regexp.MustCompile(`(?i)<br\s*/?>`)
	html = reBr.ReplaceAllString(html, "\n")
	reBlock := regexp.MustCompile(`(?i)</?(p|div|h[1-6]|li|tr|blockquote)[^>]*>`)
	html = reBlock.ReplaceAllString(html, "\n")

	// 移除所有 HTML 标签。
	reTag := regexp.MustCompile(`<[^>]+>`)
	html = reTag.ReplaceAllString(html, "")

	// 解码常见 HTML 实体。
	html = strings.ReplaceAll(html, "&amp;", "&")
	html = strings.ReplaceAll(html, "&lt;", "<")
	html = strings.ReplaceAll(html, "&gt;", ">")
	html = strings.ReplaceAll(html, "&quot;", "\"")
	html = strings.ReplaceAll(html, "&#39;", "'")
	html = strings.ReplaceAll(html, "&nbsp;", " ")

	// 压缩空白。
	reSpaces := regexp.MustCompile(`[ \t]+`)
	html = reSpaces.ReplaceAllString(html, " ")
	reNewlines := regexp.MustCompile(`\n{3,}`)
	html = reNewlines.ReplaceAllString(html, "\n\n")

	return strings.TrimSpace(html)
}

// ── WebSearch ──────────────────────────────────────────────────

// WebSearchInput 是 WebSearch 工具的输入。
type WebSearchInput struct {
	// Query 是搜索查询。
	Query string `json:"query" description:"搜索关键词"`
}

// SearchResult 是单条搜索结果。
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// SearchBackend 是搜索后端接口。
// 用户可以实现此接口来对接不同的搜索 API。
type SearchBackend interface {
	Search(ctx context.Context, query string) ([]SearchResult, error)
}

// WebSearchTool 返回 WebSearch 工具定义。
// backend 是搜索后端实现（如 BraveBackend, TavilyBackend 等）。
func WebSearchTool(backend SearchBackend) goagent.ToolDef {
	return goagent.ToolDef{
		Description: "搜索互联网并返回相关结果（标题、URL、摘要）。",
		Input:       WebSearchInput{},
		Execute: func(ctx goagent.Context, in WebSearchInput) (string, error) {
			return executeSearch(ctx, in, backend)
		},
	}
}

func executeSearch(ctx goagent.Context, in WebSearchInput, backend SearchBackend) (string, error) {
	if in.Query == "" {
		return "", fmt.Errorf("搜索关键词不能为空")
	}

	results, err := backend.Search(ctx, in.Query)
	if err != nil {
		return "", fmt.Errorf("搜索失败: %w", err)
	}

	if len(results) == 0 {
		return "未找到相关结果。", nil
	}

	// 格式化输出。
	var sb strings.Builder
	for i, r := range results {
		sb.WriteString(fmt.Sprintf("%d. %s\n   %s\n   %s\n\n", i+1, r.Title, r.URL, r.Snippet))
	}
	return sb.String(), nil
}

// ── 内置搜索后端 ──────────────────────────────────────────────

// BraveBackend 对接 Brave Search API。
type BraveBackend struct {
	APIKey string
}

// Search 实现 SearchBackend。
func (b *BraveBackend) Search(ctx context.Context, query string) ([]SearchResult, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.search.brave.com/res/v1/web/search", nil)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	q.Set("q", query)
	q.Set("count", "10")
	req.URL.RawQuery = q.Encode()
	req.Header.Set("X-Subscription-Token", b.APIKey)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("Brave API 错误 (HTTP %d): %s", resp.StatusCode, string(body))
	}

	// 解析 Brave 搜索响应。
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return nil, err
	}

	return parseBraveResults(body)
}

// parseBraveResults 从 Brave API JSON 响应中提取搜索结果。
func parseBraveResults(data []byte) ([]SearchResult, error) {
	// Brave API 返回格式：{ "web": { "results": [...] } }
	// 使用简单的 JSON 解析避免引入额外依赖。
	type braveResult struct {
		Title       string `json:"title"`
		URL         string `json:"url"`
		Description string `json:"description"`
	}
	type braveWeb struct {
		Results []braveResult `json:"results"`
	}
	type braveResponse struct {
		Web braveWeb `json:"web"`
	}

	var resp braveResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("解析 Brave 响应失败: %w", err)
	}

	var results []SearchResult
	for _, r := range resp.Web.Results {
		results = append(results, SearchResult{
			Title:   r.Title,
			URL:     r.URL,
			Snippet: r.Description,
		})
	}
	return results, nil
}

// TavilyBackend 对接 Tavily Search API。
type TavilyBackend struct {
	APIKey string
}

// Search 实现 SearchBackend。
func (t *TavilyBackend) Search(ctx context.Context, query string) ([]SearchResult, error) {
	payload := fmt.Sprintf(`{"query":%q,"max_results":10}`, query)
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.tavily.com/search", strings.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.APIKey)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("Tavily API 错误 (HTTP %d): %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return nil, err
	}

	return parseTavilyResults(body)
}

func parseTavilyResults(data []byte) ([]SearchResult, error) {
	type tavilyResult struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	}
	type tavilyResponse struct {
		Results []tavilyResult `json:"results"`
	}

	var resp tavilyResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("解析 Tavily 响应失败: %w", err)
	}

	var results []SearchResult
	for _, r := range resp.Results {
		results = append(results, SearchResult{
			Title:   r.Title,
			URL:     r.URL,
			Snippet: r.Content,
		})
	}
	return results, nil
}
