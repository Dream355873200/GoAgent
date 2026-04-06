// Example: Chatbot — 带完整内置工具的可运行示例
//
// 连接本地 Ollama 或任意 OpenAI 兼容 API，启动交互式 Agent。
// 继承 GoAgent 框架所有核心能力：
//   - 内置工具: Read/Write/Edit/Glob/Grep/Bash/AskUser + 管理工具
//   - 多轮对话历史（TUI 内存累积）
//   - 跨会话持久化内存（.goagent/memory/）
//   - 会话内定期记忆提取（SessionMemory）
//   - 上下文压缩（自动 + 手动 /compact）
//   - 权限管理（交互式审批）
//   - 项目上下文（CLAUDE.md）
//   - Claude Code 中文提示词体系
//
// 用法：
//
//	# 使用默认配置（启用全部工具）
//	go run main.go
//
//	# 使用其他模型
//	OPENAI_MODEL=qwen2.5:7b go run main.go
//
//	# 禁用工具（纯对话模式）
//	DISABLE_TOOLS=1 go run main.go
//
//	# 使用 OpenRouter
//	OPENAI_API_KEY=sk-or-... OPENAI_BASE_URL=https://openrouter.ai/api/v1 OPENAI_MODEL=anthropic/claude-3.5-sonnet go run main.go
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Dream355873200/GoAgent"
	"github.com/Dream355873200/GoAgent/builtin"
	"github.com/Dream355873200/GoAgent/sessionmem"
)

func main() {
	// 从环境变量读取配置。
	baseURL := envOr("OPENAI_BASE_URL", "https://api.deepseek.com/v1")
	model := envOr("OPENAI_MODEL", "deepseek-chat")
	apiKey := envOr("OPENAI_API_KEY", "sk-d073bcc1e2524f64af853f4640a8b531")
	disableTools := os.Getenv("DISABLE_TOOLS") == "1"

	// 工作目录。
	cwd, _ := os.Getwd()

	// 内存目录（跨会话持久化）。
	memoryDir := filepath.Join(cwd, ".goagent", "memory")

	// 会话记忆目录（会话内定期提取）。
	sessionMemDir := filepath.Join(cwd, ".goagent", "session-memory")

	fmt.Println("GoAgent Chatbot")
	fmt.Printf("  API:   %s\n", baseURL)
	fmt.Printf("  模型:  %s\n", model)
	fmt.Printf("  内存:  %s\n", memoryDir)
	if disableTools {
		fmt.Println("  工具:  已禁用（纯对话模式）")
	} else {
		fmt.Println("  工具:  已启用（全部内置工具 + 管理工具）")
	}
	fmt.Println()

	// 构建选项列表。
	opts := []goagent.Option{
		// LLM 提供者。
		goagent.ProviderConfig{
			Type:    "openai",
			Model:   model,
			APIKey:  apiKey,
			BaseURL: baseURL,
		},

		// Claude Code 中文提示词体系。
		goagent.WithClaudeCodePrompts(),

		// 最大轮次。
		goagent.WithMaxTurns(50),

		// 跨会话持久化内存（类似 CLAUDE.md 的自动记忆）。
		goagent.WithMemoryDir(memoryDir),

		// 会话内定期记忆提取（防止长对话中重要上下文在 compaction 时丢失）。
		goagent.WithSessionMemory(sessionmem.Config{
			MinTokensToInit:         10000,
			MinTokensBetweenUpdate:  5000,
			ToolCallsBetweenUpdates: 3,
			MemoryDir:               sessionMemDir,
			MaxSectionTokens:        2000,
			MaxTotalTokens:          12000,
		}),

		// 上下文压缩配置。
		goagent.WithCompaction(goagent.CompactionConfig{
			AutoCompactThreshold: 0.8,   // 80% 上下文窗口时自动压缩
			MaxResultSize:        50000, // 单个工具结果最大 50K 字符
		}),

		// 权限模式：交互式开发（ReadOnly 和 Normal 自动通过，Dangerous 需确认）。
		goagent.PermissionPresetInteractive(),

		// 启用 Task/Plan/Ask 管理工具。
		goagent.WithTaskTools(),
		goagent.WithPlanTools(),
		goagent.WithAskTools(),
	}

	// 如果存在项目级 CLAUDE.md，自动加载。
	claudeMD := filepath.Join(cwd, "CLAUDE.md")
	if _, err := os.Stat(claudeMD); err == nil {
		opts = append(opts, goagent.WithProjectContext(claudeMD))
	}

	app := goagent.New(opts...)

	// 注册核心工具。
	// Task/Plan/Ask 等管理工具已通过 WithTaskTools/WithPlanTools/WithAskTools 注册。
	app.UseTools(builtin.CoreTools()...)

	// 启动 CLI（TUI 模式）。
	app.RunCLI()
}

// envOr 从环境变量读取，如果为空返回默认值。
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
