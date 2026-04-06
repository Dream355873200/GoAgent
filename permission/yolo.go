package permission

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// YoloResult 是 YOLO 分类的结果。
type YoloResult struct {
	ShouldBlock       bool
	Reason            string
	Thinking          string
	Unavailable       bool // API 调用失败
	TranscriptTooLong bool // 超出上下文窗口
}

// YoloClassifierConfig 是 YOLO 分类器的配置。
type YoloClassifierConfig struct {
	// Model 用于分类的模型（默认使用主 provider 的模型）。
	Model string
	// Stage1MaxTokens Stage 1 快速分类的 max_tokens（默认 256）。
	Stage1MaxTokens int
	// Stage2MaxTokens Stage 2 思考分类的 max_tokens（默认 4096）。
	Stage2MaxTokens int
	// Mode: "both"(默认两阶段), "fast"(仅快速), "thinking"(仅思考)。
	Mode string
	// PromptFile 外部 prompt 文件路径（空则使用嵌入默认值）。
	PromptFile string
}

// YoloClassifier 使用 LLM 子模型调用进行权限分类。
// 对齐 Claude Code 的 yoloClassifier 机制：
//   - Stage 1（快速）：少量 token，快速判断，允许则直接通过。
//   - Stage 2（思考）：更多 token，深度推理，仅在 Stage 1 判定为 block 时触发。
type YoloClassifier struct {
	provider provider.Provider
	config   YoloClassifierConfig
}

// NewYoloClassifier 创建 YOLO 分类器。
func NewYoloClassifier(prov provider.Provider, cfg YoloClassifierConfig) *YoloClassifier {
	if cfg.Stage1MaxTokens == 0 {
		cfg.Stage1MaxTokens = 256
	}
	if cfg.Stage2MaxTokens == 0 {
		cfg.Stage2MaxTokens = 4096
	}
	if cfg.Mode == "" {
		cfg.Mode = "both"
	}
	return &YoloClassifier{provider: prov, config: cfg}
}

// Classify 对工具调用进行 YOLO 分类。
//
// messages: 近期对话记录（用于构建 transcript 上下文）。
// toolName: 待分类的工具名。
// toolInput: 待分类的工具输入（JSON 字符串）。
func (c *YoloClassifier) Classify(ctx context.Context, messages []message.Message, toolName, toolInput string) YoloResult {
	transcript := buildTranscript(messages, toolName, toolInput)

	// Stage 1: 快速分类
	if c.config.Mode == "fast" || c.config.Mode == "both" {
		result := c.runStage(ctx, transcript, 1)
		if result.Unavailable {
			return result
		}
		if !result.ShouldBlock {
			return result // Stage 1 说 no → 允许
		}
		if c.config.Mode == "fast" {
			return result // fast 模式不走 stage 2
		}
		// Stage 1 说 block → 进入 Stage 2 确认
	}

	// Stage 2: 深度思考分类
	if c.config.Mode == "thinking" || c.config.Mode == "both" {
		return c.runStage(ctx, transcript, 2)
	}

	return YoloResult{ShouldBlock: true, Reason: "unknown classifier mode"}
}

func (c *YoloClassifier) runStage(ctx context.Context, transcript string, stage int) YoloResult {
	systemPrompt := YoloSystemPrompt(c.config.PromptFile)
	maxTokens := c.config.Stage1MaxTokens

	if stage == 1 {
		systemPrompt += stage1Suffix
		maxTokens = c.config.Stage1MaxTokens
	} else {
		systemPrompt += stage2Suffix
		maxTokens = c.config.Stage2MaxTokens
	}

	model := c.config.Model
	if model == "" {
		caps := c.provider.Capabilities()
		if caps.ModelID != "" {
			model = caps.ModelID
		}
	}

	req := &provider.Request{
		SystemPrompt: systemPrompt,
		Messages: []message.Message{
			message.NewUserMessage(transcript),
		},
		MaxTokens: maxTokens,
		Model:     model,
	}

	resp, err := c.provider.Complete(ctx, req)
	if err != nil {
		if isPromptTooLong(err) {
			return YoloResult{Unavailable: false, TranscriptTooLong: true}
		}
		return YoloResult{Unavailable: true}
	}

	return parseYoloResponse(resp.Message)
}

// buildTranscript 从消息历史构建分类器输入文本。
// 对齐 Claude Code 的 transcript 构建逻辑：
//   - 用户文本消息保留
//   - 助手的 tool_use 块保留（tool_result 跳过）
//   - 助手文本跳过
//   - 最近 20 条消息避免超限
//   - 最后追加待分类的动作
func buildTranscript(messages []message.Message, toolName, toolInput string) string {
	var sb strings.Builder

	start := 0
	if len(messages) > 20 {
		start = len(messages) - 20
	}

	for _, msg := range messages[start:] {
		for _, block := range msg.Content {
			switch block.Type {
			case "text":
				if msg.Role == message.RoleUser {
					sb.WriteString(fmt.Sprintf("User: %s\n", block.Text))
				}
			case "tool_use":
				input := string(block.Input)
				if len(input) > 500 {
					input = input[:500] + "..."
				}
				sb.WriteString(fmt.Sprintf("%s %s\n", block.ToolName, input))
			}
		}
	}

	// 最后追加待分类的动作
	sb.WriteString(fmt.Sprintf("%s %s", toolName, toolInput))
	return sb.String()
}

// XML 标签正则（包级编译一次）。
var (
	reThinking = regexp.MustCompile(`<thinking>(.*?)</thinking>`)
	reBlock    = regexp.MustCompile(`<block>(yes|no)</block>`)
	reReason   = regexp.MustCompile(`<reason>(.*?)</reason>`)
)

// parseYoloResponse 解析 LLM 的 XML 输出。
// 对齐 Claude Code 的 XML 解析逻辑。
func parseYoloResponse(msg message.Message) YoloResult {
	fullText := message.ExtractText(msg)

	// 提取 thinking（可选），提取后移除避免干扰后续匹配。
	thinking := ""
	if matches := reThinking.FindStringSubmatch(fullText); len(matches) > 1 {
		thinking = matches[1]
		fullText = strings.Replace(fullText, matches[0], "", 1)
	}

	// 提取 block 决策。默认 block（对齐 Claude Code 的 fail-closed 策略）。
	shouldBlock := true
	if matches := reBlock.FindStringSubmatch(fullText); len(matches) > 1 {
		shouldBlock = matches[1] == "yes"
	}

	// 提取 reason（仅 block 时有）。
	reason := ""
	if matches := reReason.FindStringSubmatch(fullText); len(matches) > 1 {
		reason = matches[1]
	}

	return YoloResult{
		ShouldBlock: shouldBlock,
		Reason:      reason,
		Thinking:    thinking,
	}
}

func isPromptTooLong(err error) bool {
	if _, ok := err.(*provider.PromptTooLongError); ok {
		return true
	}
	return strings.Contains(err.Error(), "prompt_too_long")
}
