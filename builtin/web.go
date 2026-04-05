package builtin

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Dream355873200/GoAgent"
)

// WebSearchInput 是 WebSearch 工具的输入。
type WebSearchInput struct {
	Query string `json:"query" desc:"搜索查询词" required:"true"`
	// NumResults 返回结果数量，默认 5。
	NumResults int `json:"num_results,omitempty" desc:"返回结果数量，默认 5"`
}

// WebSearchTool 返回网络搜索工具定义。
func WebSearchTool() goagent.ToolDef {
	return goagent.ToolDef{
		Description: "执行网络搜索。返回搜索结果列表，包括标题、URL 和摘要。" +
			"用于查找最新信息、技术文档、新闻等。",
		Input:              WebSearchInput{},
		Permission:         goagent.ReadOnly,
		Concurrent:         true,
		MaxResultSizeChars: 50000,
		Execute: func(ctx goagent.Context, in WebSearchInput) (string, error) {
			return executeWebSearch(in)
		},
	}
}

func executeWebSearch(in WebSearchInput) (string, error) {
	if in.Query == "" {
		return "", fmt.Errorf("query 不能为空")
	}

	numResults := in.NumResults
	if numResults <= 0 {
		numResults = 5
	}
	if numResults > 20 {
		numResults = 20
	}

	// 使用 DuckDuckGo HTML 搜索 API（无需 API key）
	query := url.QueryEscape(in.Query)
	searchURL := fmt.Sprintf("https://html.duckduckgo.com/html/?q=%s", query)

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", searchURL, nil)
	if err != nil {
		return "", fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; GoAgent/1.0)")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("搜索请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("读取响应失败: %w", err)
	}

	// 解析 DuckDuckGo HTML 结果
	results := parseDuckDuckGoHTML(string(body), numResults)
	if len(results) == 0 {
		return "(无搜索结果)", nil
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("搜索结果 (%d 条):\n\n", len(results)))
	for i, r := range results {
		output.WriteString(fmt.Sprintf("%d. %s\n   %s\n   %s\n\n", i+1, r.Title, r.URL, r.Snippet))
	}
	return output.String(), nil
}

type searchResult struct {
	Title   string
	URL     string
	Snippet string
}

// parseDuckDuckGoHTML 解析 DuckDuckGo HTML 搜索结果。
func parseDuckDuckGoHTML(html string, maxResults int) []searchResult {
	var results []searchResult

	// 简单的 HTML 解析：查找 <a class="result__a" href="...">标题</a>
	// 和相邻的 <a class="result__snippet" href="...">摘要</a>
	lines := strings.Split(html, "\n")
	var currentTitle string
	var currentURL string

	for i := 0; i < len(lines) && len(results) < maxResults; i++ {
		line := strings.TrimSpace(lines[i])

		// 查找结果链接
		if strings.Contains(line, `class="result__a"`) {
			hrefStart := strings.Index(line, `href="`)
			if hrefStart != -1 {
				hrefEnd := strings.Index(line[hrefStart+6:], `"`)
				if hrefEnd != -1 {
					currentURL = line[hrefStart+6 : hrefStart+6+hrefEnd]
				}
			}
			titleStart := strings.Index(line, ">")
			titleEnd := strings.LastIndex(line, "<")
			if titleStart != -1 && titleEnd != -1 && titleStart < titleEnd {
				currentTitle = strings.TrimSpace(line[titleStart+1 : titleEnd])
			}
		}

		// 查找摘要
		if strings.Contains(line, `class="result__snippet"`) && currentTitle != "" {
			snippetStart := strings.Index(line, ">")
			snippetEnd := strings.LastIndex(line, "<")
			if snippetStart != -1 && snippetEnd != -1 && snippetStart < snippetEnd {
				snippet := strings.TrimSpace(line[snippetStart+1 : snippetEnd])
				// 移除 HTML 标签
				snippet = strings.ReplaceAll(snippet, "<b>", "")
				snippet = strings.ReplaceAll(snippet, "</b>", "")
				snippet = strings.ReplaceAll(snippet, "&amp;", "&")
				snippet = strings.ReplaceAll(snippet, "&quot;", "\"")
				snippet = strings.ReplaceAll(snippet, "&#39;", "'")
				snippet = strings.ReplaceAll(snippet, "&lt;", "<")
				snippet = strings.ReplaceAll(snippet, "&gt;", ">")

				results = append(results, searchResult{
					Title:   currentTitle,
					URL:     currentURL,
					Snippet: snippet,
				})
				currentTitle = ""
				currentURL = ""
			}
		}
	}

	return results
}

// WebFetchInput 是 WebFetch 工具的输入。
type WebFetchInput struct {
	URL         string `json:"url" desc:"要获取的 URL" required:"true"`
	Description string `json:"description,omitempty" desc:"页面描述/用途"`
	// MaxLength 最大获取字符数，默认 50000。
	MaxLength int `json:"max_length,omitempty" desc:"最大获取字符数，默认 50000"`
}

// WebFetchTool 返回网页获取工具定义。
func WebFetchTool() goagent.ToolDef {
	return goagent.ToolDef{
		Description: "获取并解析网页内容。返回页面的文本内容（去除了 HTML 标签）。" +
			"用于获取文档、博客文章、技术资料等。",
		Input:              WebFetchInput{},
		Permission:         goagent.ReadOnly,
		Concurrent:         true,
		MaxResultSizeChars: 100000,
		Execute: func(ctx goagent.Context, in WebFetchInput) (string, error) {
			return executeWebFetch(in)
		},
	}
}

func executeWebFetch(in WebFetchInput) (string, error) {
	if in.URL == "" {
		return "", fmt.Errorf("url 不能为空")
	}

	// 验证 URL
	parsedURL, err := url.Parse(in.URL)
	if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
		return "", fmt.Errorf("无效的 URL: %s", in.URL)
	}

	maxLength := in.MaxLength
	if maxLength <= 0 {
		maxLength = 50000
	}
	if maxLength > 200000 {
		maxLength = 200000
	}

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", in.URL, nil)
	if err != nil {
		return "", fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; GoAgent/1.0)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("获取页面失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxLength+1000)))
	if err != nil {
		return "", fmt.Errorf("读取页面失败: %w", err)
	}

	contentType := resp.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "text/html") {
		// 提取纯文本
		text := extractTextFromHTML(string(body))
		if len(text) > maxLength {
			text = text[:maxLength] + "..."
		}
		return text, nil
	}

	// 非 HTML 内容直接返回
	result := string(body)
	if len(result) > maxLength {
		result = result[:maxLength] + "..."
	}
	return result, nil
}

// extractTextFromHTML 从 HTML 中提取纯文本。
func extractTextFromHTML(html string) string {
	// 移除 script 和 style 标签及其内容
	html = removeTagAndContent(html, "script")
	html = removeTagAndContent(html, "style")
	html = removeTagAndContent(html, "noscript")

	// 替换块级标签为空格
	blockTags := []string{"br", "p", "div", "h1", "h2", "h3", "h4", "h5", "h6", "li", "tr", "table"}
	for _, tag := range blockTags {
		html = strings.ReplaceAll(html, "<"+tag, " <"+tag)
		html = strings.ReplaceAll(html, "</"+tag+">", " ")
	}

	// 移除所有 HTML 标签
	var result strings.Builder
	inTag := false
	for _, r := range html {
		if r == '<' {
			inTag = true
			result.WriteRune(' ')
		} else if r == '>' {
			inTag = false
			result.WriteRune(' ')
		} else if !inTag {
			result.WriteRune(r)
		}
	}

	// 清理空白
	text := result.String()
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.ReplaceAll(text, "\r", " ")
	text = strings.ReplaceAll(text, "\t", " ")

	// 合并多个空格
	for strings.Contains(text, "  ") {
		text = strings.ReplaceAll(text, "  ", " ")
	}
	text = strings.TrimSpace(text)

	// HTML 实体解码
	text = decodeHTMLEntities(text)

	return text
}

// removeTagAndContent 移除指定标签及其内容。
func removeTagAndContent(html, tag string) string {
	start := 0
	for {
		tagStart := strings.Index(html[start:], "<"+tag)
		if tagStart == -1 {
			break
		}
		tagStart += start

		// 检查是否是闭合标签
		if html[tagStart+1] == '/' {
			start = tagStart + 1
			continue
		}

		// 查找标签结束
		tagEnd := strings.Index(html[tagStart:], ">")
		if tagEnd == -1 {
			break
		}
		tagEnd += tagStart

		// 查找闭合标签
		closeStart := strings.Index(html[tagEnd:], "</"+tag+">")
		if closeStart == -1 {
			break
		}
		closeStart += tagEnd
		closeEnd := closeStart + len("</"+tag+">")

		html = html[:tagStart] + html[closeEnd:]
		start = tagStart
	}
	return html
}

// decodeHTMLEntities 解码常见 HTML 实体。
func decodeHTMLEntities(text string) string {
	replacements := map[string]string{
		"&amp;":  "&",
		"&quot;": "\"",
		"&#39;":  "'",
		"&lt;":   "<",
		"&gt;":   ">",
		"&nbsp;": " ",
		"&#x27;": "'",
		"&#x2F;": "/",
	}
	for entity, replacement := range replacements {
		text = strings.ReplaceAll(text, entity, replacement)
	}
	return text
}
