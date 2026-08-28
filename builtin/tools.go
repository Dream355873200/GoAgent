// Package builtin 提供框架内置工具。
//
// 包含 Claude Code 核心工具的 Go 实现：
// Read, Write, Edit, Glob, Grep, Bash。
// 用户可通过 builtin.CoreTools() 一次注册核心工具。
package builtin

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Dream355873200/GoAgent"
	"github.com/Dream355873200/GoAgent/agent"
	"github.com/Dream355873200/GoAgent/bgtask"
	"github.com/Dream355873200/GoAgent/plan"
	"github.com/Dream355873200/GoAgent/provider"
	"github.com/Dream355873200/GoAgent/task"
)

func init() {
	// 注册内置工具提供函数，使 WithBuiltinTools() 可以工作。
	goagent.RegisterBuiltinToolsProvider(func() []goagent.NamedTool {
		return CoreTools()
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

	// 注册 Issue 工具提供函数（WithIssueTools() 启用；与 task 同存储）。
	goagent.RegisterIssueToolsProvider(func(store task.StoreInterface) []goagent.NamedTool {
		return []goagent.NamedTool{
			{Name: "IssueReport", Def: IssueReportTool(store)},
			{Name: "IssueResolve", Def: IssueResolveTool(store)},
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

	// 注册 AskUser 工具提供函数。
	goagent.RegisterAskToolsProvider(func() []goagent.NamedTool {
		return []goagent.NamedTool{
			{Name: "AskUser", Def: AskUserTool()},
		}
	})
}

// CoreTools 返回核心内置工具，供 app.UseTools() 批量注册。
// 包含 Read/Write/Edit/Glob/Grep/Bash/WebSearch/WebFetch。
// 不包含 AskUser/Task/Plan/BgTask 等子系统工具，需通过对应 With* Option 单独启用。
//
// AllTools 是 CoreTools 的别名（向后兼容）。
func CoreTools() []goagent.NamedTool {
	return []goagent.NamedTool{
		{Name: "Read", Def: ReadTool()},
		{Name: "Write", Def: WriteTool()},
		{Name: "Edit", Def: EditTool()},
		{Name: "Glob", Def: GlobTool()},
		{Name: "Grep", Def: GrepTool()},
		{Name: "Bash", Def: BashTool()},
		{Name: "GitCommit", Def: GitCommitTool()},
		{Name: "WebSearch", Def: WebSearchTool()},
		{Name: "WebFetch", Def: WebFetchTool()},
	}
}

// AllTools 是 CoreTools 的别名，向后兼容。
// Deprecated: 请使用 CoreTools()。
func AllTools() []goagent.NamedTool { return CoreTools() }

// resolvePath 把相对路径解析到会话工作目录（WithSessionWorkDir 注入）。
// 绝对路径原样返回；未注入工作目录时保持相对路径，由 OS 按进程 cwd 解析（旧行为）。
func resolvePath(ctx context.Context, p string) string {
	wd := workDirOf(ctx)
	if p == "" || wd == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(wd, p)
}

// resolveRoot 返回搜索根目录：显式 path > 会话工作目录 > 进程 cwd。
func resolveRoot(ctx context.Context, explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if wd := workDirOf(ctx); wd != "" {
		return wd, nil
	}
	return os.Getwd()
}

// workDirOf nil 安全的会话工作目录提取。
func workDirOf(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	return goagent.WorkDirFromContext(ctx)
}

// sandboxOf 双路提取沙箱会话：优先 goagent.Context 结构体的 Sandbox 字段
// （ToolDef.call 真实路径提升自 context value；手工构造 Context 的调用方
// 直接设字段即可），否则从 context value 链提取。
func sandboxOf(ctx context.Context) goagent.SandboxSession {
	if gctx, ok := ctx.(goagent.Context); ok && gctx.Sandbox != nil {
		return gctx.Sandbox
	}
	return goagent.SandboxFromContext(ctx)
}

// resolvePathChecked 沙箱感知的路径解析：沙箱在场时由沙箱根解析并做
// 策略检查（违规返回 LLM 可读错误，模型可改用合法路径重试）；
// 无沙箱时走原 resolvePath（行为不变）。
func resolvePathChecked(ctx context.Context, p string, op goagent.FSOp) (string, error) {
	if sb := sandboxOf(ctx); sb != nil {
		return sb.ResolvePath(p, op)
	}
	return resolvePath(ctx, p), nil
}

// resolveRootChecked 沙箱感知的搜索根解析：沙箱在场时默认根 = 沙箱根，
// 显式路径经沙箱策略检查；无沙箱时走原 resolveRoot。
func resolveRootChecked(ctx context.Context, explicit string) (string, error) {
	if sb := sandboxOf(ctx); sb != nil {
		if explicit == "" {
			return sb.Root(), nil
		}
		return sb.ResolvePath(explicit, goagent.OpRead)
	}
	return resolveRoot(ctx, explicit)
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
		Description: "读取本地文件内容。返回带行号的文本。支持 offset/limit 分段读取大文件。" +
			"超长行（>2000 字符）会中间截断；二进制文件返回类型提示而不是乱码。" +
			"同一会话内重复读取未修改的文件会短路返回（自上次读取无变化）——需要重读时说明原因。",
		Input:      ReadInput{},
		Permission: goagent.ReadOnly,
		Concurrent: true,
		Execute: func(ctx goagent.Context, in ReadInput) (string, error) {
			p, err := resolvePathChecked(ctx, in.FilePath, goagent.OpRead)
			if err != nil {
				return "", err
			}
			in.FilePath = p
			return executeRead(ctx, in)
		},
	}
}

// 单行截断阈值（对齐 Claude Code）：超长行挖空中间，防 minified JS /
// 嵌入数据把上下文撑爆。
const maxLineLen = 2000

// 二进制检测采样字节数。
const binarySniffLen = 8000

func executeRead(ctx goagent.Context, in ReadInput) (string, error) {
	if in.FilePath == "" {
		return "", fmt.Errorf("file_path 不能为空")
	}

	raw, err := os.ReadFile(in.FilePath)
	if err != nil {
		return "", fmt.Errorf("无法打开文件: %w", err)
	}

	// 二进制检测：采样段里 NUL 比例高或不可打印控制字符多 → 不当文本读。
	if isBinary(raw[:min(len(raw), binarySniffLen)]) {
		return fmt.Sprintf("(%s 是二进制文件, %d 字节 — 无法作为文本读取)", filepath.Base(in.FilePath), len(raw)), nil
	}

	// 未修改短路：同会话内指纹一致时直接告知，防模型反复重读同一文件。
	if unchangedSinceRead(ctx.SessionID, in.FilePath, string(raw)) {
		return "(文件自上次读取后未修改 — 无需重读)", nil
	}
	markRead(ctx.SessionID, in.FilePath, string(raw))

	content := string(raw)
	limit := in.Limit
	if limit <= 0 {
		limit = 2000
	}
	offset := in.Offset
	if offset < 1 {
		offset = 1
	}

	scanner := bufio.NewScanner(strings.NewReader(content))
	// 增大缓冲区以处理长行。
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

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
		line := scanner.Text()
		if len(line) > maxLineLen {
			// 中间挖空：保住行首尾（缩进/结尾语义），截掉中间大块
			head := line[:maxLineLen/2]
			tail := line[len(line)-maxLineLen/4:]
			line = head + fmt.Sprintf(" [...truncated %d chars...] ", len(line)-maxLineLen/2-maxLineLen/4) + tail
		}
		lines = append(lines, fmt.Sprintf("%6d\t%s", lineNum, line))
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

// isBinary 粗判二进制：NUL 字节出现或不可打印控制字符占比过高。
func isBinary(sample []byte) bool {
	if len(sample) == 0 {
		return false
	}
	nul, ctrl := 0, 0
	for _, b := range sample {
		if b == 0 {
			nul++
			if nul > 1 {
				return true // 文本里 NUL 几乎不会出现一次以上
			}
		} else if b < 0x09 || (b > 0x0d && b < 0x20) {
			ctrl++
		}
	}
	return float64(ctrl)/float64(len(sample)) > 0.30
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
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
		Description: "创建或覆盖文件。将 content 写入指定路径。如果父目录不存在会自动创建。" +
			"覆盖已存在的文件前必须先 Read 它（防止凭旧印象覆盖丢改动）；新建文件无需先读。",
		Input:      WriteInput{},
		Permission: goagent.Normal,
		Execute: func(ctx goagent.Context, in WriteInput) (string, error) {
			p, err := resolvePathChecked(ctx, in.FilePath, goagent.OpWrite)
			if err != nil {
				return "", err
			}
			in.FilePath = p
			return executeWrite(ctx, in)
		},
	}
}

func executeWrite(ctx goagent.Context, in WriteInput) (string, error) {
	if in.FilePath == "" {
		return "", fmt.Errorf("file_path 不能为空")
	}

	// 覆盖保护：目标文件已存在但本会话没读过 → 拒绝并指路。
	// 模型有最新视图时才允许覆盖（对齐 Claude Code 的安全语义）。
	if _, err := os.Stat(in.FilePath); err == nil && !hasRead(ctx.SessionID, in.FilePath) {
		return "", fmt.Errorf("%s 已存在但本会话未读取过——先 Read 它确认当前内容，再决定覆盖（防止凭印象覆盖丢失现有内容）", in.FilePath)
	}

	// 确保父目录存在。
	dir := filepath.Dir(in.FilePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("无法创建目录 %s: %w", dir, err)
	}

	if err := os.WriteFile(in.FilePath, []byte(in.Content), 0644); err != nil {
		return "", fmt.Errorf("写入文件失败: %w", err)
	}
	// 写后仍标记已读（内容=模型刚写的，它有最新视图；同批后续 Edit 放行）
	markWritten(ctx.SessionID, in.FilePath, in.Content)

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
		Description: "在文件中执行精确字符串替换。old_string 必须在文件中存在且唯一" +
			"（不唯一时提供更多上下文行使其唯一，或 replace_all=true 替换全部）。" +
			"编辑前必须先 Read 该文件（本会话内）——Edit 匹配的是文件的最新内容，" +
			"凭记忆编辑会匹配失败。多处修改尽量合并成一次大块替换而不是多次小改。",
		Input:      EditInput{},
		Permission: goagent.Normal,
		Execute: func(ctx goagent.Context, in EditInput) (string, error) {
			p, err := resolvePathChecked(ctx, in.FilePath, goagent.OpWrite)
			if err != nil {
				return "", err
			}
			in.FilePath = p
			return executeEdit(ctx, in)
		},
	}
}

func executeEdit(ctx goagent.Context, in EditInput) (string, error) {
	if in.FilePath == "" {
		return "", fmt.Errorf("file_path 不能为空")
	}
	if in.OldString == in.NewString {
		return "", fmt.Errorf("old_string 和 new_string 相同，无需替换")
	}

	// 前置校验：本会话未读过该文件 → 拒绝（对齐 Claude Code）。
	// 模型没建立文件最新视图就编辑，old_string 大概率失配。
	if !hasRead(ctx.SessionID, in.FilePath) {
		return "", fmt.Errorf("%s 本会话尚未读取——先 Read 再 Edit（old_string 必须匹配文件最新内容，凭记忆编辑会失败）", in.FilePath)
	}

	content, err := os.ReadFile(in.FilePath)
	if err != nil {
		return "", fmt.Errorf("无法读取文件: %w", err)
	}

	text := string(content)
	count := strings.Count(text, in.OldString)

	if count == 0 {
		// 给出定位线索（对齐 Claude Code 的诊断式报错）：
		// 找最相似的行帮模型快速修正，而不是裸报「未找到」。
		return "", fmt.Errorf("old_string 在文件中未找到。常见原因：%s。重新 Read 文件核对最新内容后再试",
			editMissReason(text, in.OldString))
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
	// 改后仍标记已读（同批后续 Edit 放行——模型知道刚改的内容）
	markWritten(ctx.SessionID, in.FilePath, newText)

	return fmt.Sprintf("已替换 %d 处匹配 (%s)", count, in.FilePath), nil
}

// editMissReason old_string 未命中时的快速诊断。
func editMissReason(text, old string) string {
	// 空白差异（缩进/行尾）：最常见的失配原因
	normalizedOld := strings.Join(strings.Fields(old), " ")
	for _, line := range strings.Split(text, "\n") {
		if strings.Join(strings.Fields(line), " ") == normalizedOld && normalizedOld != "" {
			return "内容存在但空白/缩进不同（逐字符复制目标行，不要手敲）"
		}
	}
	// 部分命中：old 的某一行出现在文件里 → 大概率行号漂移或上下文行不对
	if oldLines := strings.Split(old, "\n"); len(oldLines) > 1 {
		for _, ol := range oldLines {
			if strings.TrimSpace(ol) != "" && strings.Contains(text, ol) {
				return "部分行存在——上下文行与文件当前内容不一致（文件可能已被修改，重新 Read）"
			}
		}
	}
	return "目标文本不在当前文件中（文件可能已被修改——重新 Read 核对）"
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
			return executeGlob(ctx, in)
		},
	}
}

func executeGlob(ctx goagent.Context, in GlobInput) (string, error) {
	if in.Pattern == "" {
		return "", fmt.Errorf("pattern 不能为空")
	}

	root, err := resolveRootChecked(ctx, in.Path)
	if err != nil {
		return "", err
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

	searchPath, err := resolveRootChecked(ctx, in.Path)
	if err != nil {
		return "", err
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
