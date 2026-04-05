// Package builtin 提供框架内置工具。
//
// 包含 Claude Code 核心工具的 Go 实现：
// Read, Write, Edit, Glob, Grep, Bash。
// 用户可通过 builtin.AllTools() 一次注册全部内置工具。
package builtin

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/anthropic-community/goagent"
	"github.com/anthropic-community/goagent/agent"
	"github.com/anthropic-community/goagent/bgtask"
	"github.com/anthropic-community/goagent/plan"
	"github.com/anthropic-community/goagent/provider"
	"github.com/anthropic-community/goagent/task"
)

func init() {
	// 注册内置工具提供函数，使 WithBuiltinTools() 可以工作。
	goagent.RegisterBuiltinToolsProvider(func() []goagent.NamedTool {
		return AllTools()
	})

	// 注册 AskUser 回调设置函数（供 TUI 模式使用）。
	goagent.RegisterSetAskUserCallback(SetAskUserCallback)

	// 注册 Task 工具提供函数。
	goagent.RegisterTaskToolsProvider(func(store task.StoreInterface) []goagent.NamedTool {
		return []goagent.NamedTool{
			{Name: "TaskCreate", Def: TaskCreateTool(store)},
			{Name: "TaskUpdate", Def: TaskUpdateTool(store)},
			{Name: "TaskGet", Def: TaskGetTool(store)},
			{Name: "TaskList", Def: TaskListTool(store)},
		}
	})

	// 注册 Plan 工具提供函数。
	goagent.RegisterPlanToolsProvider(func(store plan.StoreInterface) []goagent.NamedTool {
		return []goagent.NamedTool{
			{Name: "EnterPlanMode", Def: EnterPlanModeTool(store)},
			{Name: "ExitPlanMode", Def: ExitPlanModeTool(store)},
		}
	})

	// 注册子 agent 工具提供函数。
	goagent.RegisterSubAgentToolsProvider(func(prov provider.Provider, defs []agent.Definition) []goagent.NamedTool {
		runner := agent.NewRunner(prov)
		var tools []goagent.NamedTool
		for _, def := range defs {
			d := def // 闭包捕获
			tools = append(tools, goagent.NamedTool{
				Name: "Agent_" + d.Name,
				Def: goagent.ToolDef{
					Description: fmt.Sprintf("启动子 agent '%s' 执行独立任务。%s\n"+
						"子 agent 拥有独立的上下文和工具集。", d.Name, d.Description),
					Input:      agent.AgentToolInput{},
					Permission: goagent.Normal,
					Concurrent: true,
					Execute: func(ctx goagent.Context, in agent.AgentToolInput) (string, error) {
						if in.Prompt == "" {
							return "", fmt.Errorf("prompt 不能为空")
						}
						result, err := runner.Run(ctx, d, in.Prompt)
						if err != nil {
							return "", fmt.Errorf("子 agent '%s' 执行失败: %w", d.Name, err)
						}
						return fmt.Sprintf("%s\n\n--- 子 agent '%s': %d 轮, %d+%d tokens ---",
							result.FinalText, d.Name, result.TurnCount,
							result.Usage.InputTokens, result.Usage.OutputTokens), nil
					},
				},
			})
		}
		return tools
	})

	// 注册后台任务工具提供函数。
	goagent.RegisterBgTaskToolsProvider(func(store bgtask.StoreInterface) []goagent.NamedTool {
		return []goagent.NamedTool{
			{Name: "TaskStop", Def: TaskStopTool(store)},
			{Name: "TaskOutput", Def: TaskOutputTool(store)},
		}
	})
}

// AllTools 返回全部内置工具，供 app.UseTools() 批量注册。
func AllTools() []goagent.NamedTool {
	return []goagent.NamedTool{
		{Name: "Read", Def: ReadTool()},
		{Name: "Write", Def: WriteTool()},
		{Name: "Edit", Def: EditTool()},
		{Name: "Glob", Def: GlobTool()},
		{Name: "Grep", Def: GrepTool()},
		{Name: "Bash", Def: BashTool()},
		{Name: "AskUser", Def: AskUserTool()},
		{Name: "WebSearch", Def: WebSearchTool()},
		{Name: "WebFetch", Def: WebFetchTool()},
	}
}

// ---------- Read ----------

// ReadInput 是 Read 工具的输入。
type ReadInput struct {
	FilePath string `json:"file_path" desc:"要读取的文件绝对路径" required:"true"`
	Offset   int    `json:"offset,omitempty" desc:"起始行号（从 1 开始）"`
	Limit    int    `json:"limit,omitempty" desc:"读取的行数，默认 2000"`
}

// ReadTool 返回文件读取工具定义。
func ReadTool() goagent.ToolDef {
	return goagent.ToolDef{
		Description: "读取本地文件内容。返回带行号的文本。支持 offset/limit 分段读取大文件。",
		Input:       ReadInput{},
		Permission:  goagent.ReadOnly,
		Concurrent:  true,
		Execute: func(ctx goagent.Context, in ReadInput) (string, error) {
			return executeRead(in)
		},
	}
}

func executeRead(in ReadInput) (string, error) {
	if in.FilePath == "" {
		return "", fmt.Errorf("file_path 不能为空")
	}

	f, err := os.Open(in.FilePath)
	if err != nil {
		return "", fmt.Errorf("无法打开文件: %w", err)
	}
	defer f.Close()

	limit := in.Limit
	if limit <= 0 {
		limit = 2000
	}
	offset := in.Offset
	if offset < 1 {
		offset = 1
	}

	scanner := bufio.NewScanner(f)
	// 增大缓冲区以处理长行。
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var lines []string
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if lineNum < offset {
			continue
		}
		if lineNum >= offset+limit {
			break
		}
		lines = append(lines, fmt.Sprintf("%6d\t%s", lineNum, scanner.Text()))
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("读取文件出错: %w", err)
	}

	if len(lines) == 0 {
		if lineNum == 0 {
			return "(空文件)", nil
		}
		return fmt.Sprintf("(文件共 %d 行，offset %d 超出范围)", lineNum, offset), nil
	}

	return strings.Join(lines, "\n"), nil
}

// ---------- Write ----------

// WriteInput 是 Write 工具的输入。
type WriteInput struct {
	FilePath string `json:"file_path" desc:"要写入的文件绝对路径" required:"true"`
	Content  string `json:"content" desc:"要写入的完整内容" required:"true"`
}

// WriteTool 返回文件写入工具定义。
func WriteTool() goagent.ToolDef {
	return goagent.ToolDef{
		Description: "创建或覆盖文件。将 content 写入指定路径。如果父目录不存在会自动创建。",
		Input:       WriteInput{},
		Permission:  goagent.Normal,
		Execute: func(ctx goagent.Context, in WriteInput) (string, error) {
			return executeWrite(in)
		},
	}
}

func executeWrite(in WriteInput) (string, error) {
	if in.FilePath == "" {
		return "", fmt.Errorf("file_path 不能为空")
	}

	// 确保父目录存在。
	dir := filepath.Dir(in.FilePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("无法创建目录 %s: %w", dir, err)
	}

	if err := os.WriteFile(in.FilePath, []byte(in.Content), 0644); err != nil {
		return "", fmt.Errorf("写入文件失败: %w", err)
	}

	lines := strings.Count(in.Content, "\n") + 1
	return fmt.Sprintf("已写入 %s (%d 行, %d 字节)", in.FilePath, lines, len(in.Content)), nil
}

// ---------- Edit ----------

// EditInput 是 Edit 工具的输入。
type EditInput struct {
	FilePath   string `json:"file_path" desc:"要编辑的文件绝对路径" required:"true"`
	OldString  string `json:"old_string" desc:"要替换的原文本（必须在文件中唯一）" required:"true"`
	NewString  string `json:"new_string" desc:"替换后的新文本" required:"true"`
	ReplaceAll bool   `json:"replace_all,omitempty" desc:"替换所有匹配项（默认 false，只替换第一个且要求唯一）"`
}

// EditTool 返回精确字符串替换工具定义。
func EditTool() goagent.ToolDef {
	return goagent.ToolDef{
		Description: "在文件中执行精确字符串替换。old_string 必须在文件中存在。" +
			"默认要求 old_string 唯一（只出现一次），设置 replace_all=true 可替换所有匹配。",
		Input:      EditInput{},
		Permission: goagent.Normal,
		Execute: func(ctx goagent.Context, in EditInput) (string, error) {
			return executeEdit(in)
		},
	}
}

func executeEdit(in EditInput) (string, error) {
	if in.FilePath == "" {
		return "", fmt.Errorf("file_path 不能为空")
	}
	if in.OldString == in.NewString {
		return "", fmt.Errorf("old_string 和 new_string 相同，无需替换")
	}

	content, err := os.ReadFile(in.FilePath)
	if err != nil {
		return "", fmt.Errorf("无法读取文件: %w", err)
	}

	text := string(content)
	count := strings.Count(text, in.OldString)

	if count == 0 {
		return "", fmt.Errorf("old_string 在文件中未找到")
	}

	if !in.ReplaceAll && count > 1 {
		return "", fmt.Errorf("old_string 在文件中出现 %d 次（非唯一）。请提供更多上下文使其唯一，或设置 replace_all=true", count)
	}

	var newText string
	if in.ReplaceAll {
		newText = strings.ReplaceAll(text, in.OldString, in.NewString)
	} else {
		newText = strings.Replace(text, in.OldString, in.NewString, 1)
	}

	if err := os.WriteFile(in.FilePath, []byte(newText), 0644); err != nil {
		return "", fmt.Errorf("写入文件失败: %w", err)
	}

	return fmt.Sprintf("已替换 %d 处匹配 (%s)", count, in.FilePath), nil
}

// ---------- Glob ----------

// GlobInput 是 Glob 工具的输入。
type GlobInput struct {
	Pattern string `json:"pattern" desc:"glob 模式，如 **/*.go 或 src/**/*.ts" required:"true"`
	Path    string `json:"path,omitempty" desc:"搜索的根目录，默认为当前目录"`
}

// GlobTool 返回文件搜索工具定义。
func GlobTool() goagent.ToolDef {
	return goagent.ToolDef{
		Description:        "按 glob 模式搜索文件。返回匹配的文件路径列表，按修改时间排序。",
		Input:              GlobInput{},
		Permission:         goagent.ReadOnly,
		Concurrent:         true,
		MaxResultSizeChars: 50000,
		Execute: func(ctx goagent.Context, in GlobInput) (string, error) {
			return executeGlob(in)
		},
	}
}

func executeGlob(in GlobInput) (string, error) {
	if in.Pattern == "" {
		return "", fmt.Errorf("pattern 不能为空")
	}

	root := in.Path
	if root == "" {
		var err error
		root, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("无法获取当前目录: %w", err)
		}
	}

	// 处理 ** 递归模式。
	pattern := in.Pattern
	if strings.Contains(pattern, "**") {
		return executeGlobRecursive(root, pattern)
	}

	// 简单 glob。
	fullPattern := filepath.Join(root, pattern)
	matches, err := filepath.Glob(fullPattern)
	if err != nil {
		return "", fmt.Errorf("glob 模式错误: %w", err)
	}

	return formatGlobResults(matches, root)
}

// executeGlobRecursive 处理含 ** 的递归 glob。
func executeGlobRecursive(root, pattern string) (string, error) {
	// 将 **/*.ext 拆分为 目录遍历 + 文件匹配。
	// 简化实现：遍历所有文件，用 filepath.Match 过滤。
	parts := strings.SplitN(pattern, "**", 2)
	prefix := parts[0]
	suffix := ""
	if len(parts) > 1 {
		suffix = strings.TrimPrefix(parts[1], "/")
		suffix = strings.TrimPrefix(suffix, string(filepath.Separator))
	}

	searchRoot := filepath.Join(root, prefix)
	if prefix == "" {
		searchRoot = root
	}

	var matches []string
	err := filepath.Walk(searchRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // 跳过无法访问的目录。
		}
		if info.IsDir() {
			// 跳过隐藏目录和常见忽略目录。
			name := info.Name()
			if strings.HasPrefix(name, ".") || name == "node_modules" || name == "__pycache__" || name == "vendor" {
				if path != searchRoot {
					return filepath.SkipDir
				}
			}
			return nil
		}

		if suffix == "" {
			matches = append(matches, path)
			return nil
		}

		// 检查文件名是否匹配 suffix 模式。
		matched, _ := filepath.Match(suffix, info.Name())
		if matched {
			matches = append(matches, path)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("遍历目录失败: %w", err)
	}

	return formatGlobResults(matches, root)
}

// formatGlobResults 格式化 glob 结果，按修改时间排序。
func formatGlobResults(matches []string, root string) (string, error) {
	if len(matches) == 0 {
		return "(无匹配文件)", nil
	}

	// 按修改时间排序（最新在前）。
	type fileEntry struct {
		path    string
		modTime int64
	}
	entries := make([]fileEntry, 0, len(matches))
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil {
			continue
		}
		entries = append(entries, fileEntry{path: m, modTime: info.ModTime().UnixNano()})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].modTime > entries[j].modTime
	})

	var lines []string
	for _, e := range entries {
		rel, err := filepath.Rel(root, e.path)
		if err != nil {
			rel = e.path
		}
		lines = append(lines, rel)
	}

	result := strings.Join(lines, "\n")
	if len(entries) > 0 {
		result = fmt.Sprintf("(%d 个文件)\n%s", len(entries), result)
	}
	return result, nil
}

// ---------- Grep ----------

// GrepInput 是 Grep 工具的输入。
type GrepInput struct {
	Pattern    string `json:"pattern" desc:"正则表达式搜索模式" required:"true"`
	Path       string `json:"path,omitempty" desc:"搜索的文件或目录，默认当前目录"`
	Glob       string `json:"glob,omitempty" desc:"文件过滤 glob 模式，如 *.go"`
	ContextN   int    `json:"context,omitempty" desc:"显示匹配行前后各 N 行"`
	IgnoreCase bool   `json:"-i,omitempty" desc:"忽略大小写"`
}

// GrepTool 返回内容搜索工具定义。
func GrepTool() goagent.ToolDef {
	return goagent.ToolDef{
		Description:        "在文件内容中搜索正则表达式模式。返回匹配的文件路径和行内容。",
		Input:              GrepInput{},
		Permission:         goagent.ReadOnly,
		Concurrent:         true,
		MaxResultSizeChars: 50000,
		Execute: func(ctx goagent.Context, in GrepInput) (string, error) {
			return executeGrep(ctx, in)
		},
	}
}

func executeGrep(ctx context.Context, in GrepInput) (string, error) {
	if in.Pattern == "" {
		return "", fmt.Errorf("pattern 不能为空")
	}

	searchPath := in.Path
	if searchPath == "" {
		var err error
		searchPath, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("无法获取当前目录: %w", err)
		}
	}

	// 构建 grep/rg 命令参数。优先用 rg（如果可用）。
	args := buildGrepArgs(in, searchPath)

	// 使用 exec 执行（复用 Bash 逻辑）。
	result, err := runCommand(ctx, args[0], args[1:]...)
	if err != nil {
		// grep 返回 1 表示无匹配，不是错误。
		if result != "" {
			return result, nil
		}
		return "(无匹配结果)", nil
	}

	if result == "" {
		return "(无匹配结果)", nil
	}
	return result, nil
}

// buildGrepArgs 构建 grep 命令参数。
func buildGrepArgs(in GrepInput, searchPath string) []string {
	// 优先使用 rg（ripgrep），如果不可用则回退到 grep。
	args := []string{"grep", "-rn"}

	if in.IgnoreCase {
		args = append(args, "-i")
	}
	if in.ContextN > 0 {
		args = append(args, fmt.Sprintf("-C%d", in.ContextN))
	}
	if in.Glob != "" {
		args = append(args, "--include="+in.Glob)
	}

	// 排除常见无关目录。
	args = append(args, "--exclude-dir=.git", "--exclude-dir=node_modules",
		"--exclude-dir=__pycache__", "--exclude-dir=vendor", "--exclude-dir=.claude")

	args = append(args, in.Pattern, searchPath)
	return args
}
