// Package message — Token 估算工具。
//
// 提供类型感知的 token 估算，对齐 Claude Code 的 tokenEstimation.ts。
// 不同内容类型有不同的 token 密度。
package message

// Token 估算常量，来源于 Claude Code 的 tokenEstimation.ts。
const (
	// CharsPerToken 文本内容的基础比率。
	CharsPerToken = 4

	// JSONCharsPerToken 结构化内容（JSON）的比率。
	// JSON 由于字段名、引号、大括号等更加 token 密集。
	JSONCharsPerToken = 2

	// ImageTokenEstimate 图片块的固定 token 估算值。
	// Claude Code 对每张图片统一使用 2000 tokens，不论大小。
	ImageTokenEstimate = 2000

	// DocumentTokenEstimate 文档块的固定 token 估算值。
	DocumentTokenEstimate = 2000

	// ThinkingCharsPerToken 思维块的每字符 token 比率。
	// 思维内容通常比普通文本更密集。
	ThinkingCharsPerToken = 3

	// MessageOverhead 每条消息的帧开销（tokens）。
	// 包含角色标识、消息边界等。
	MessageOverhead = 10

	// PaddingNumerator 和 PaddingDenominator 实现 4/3 安全填充因子。
	// Claude Code 使用 1.33x 因子防止低估。
	PaddingNumerator   = 4
	PaddingDenominator = 3

	// ReservedSummaryTokens 从上下文窗口中减去的预留 token 数，
	// 用于压缩时的摘要输出。对齐 Claude Code 的 20k 预留。
	ReservedSummaryTokens = 20_000

	// Token 警告阈值，以有效上下文窗口的比例表示。
	WarningThreshold  = 0.80 // 80% 使用率 → 警告
	ErrorThreshold    = 0.90 // 90% 使用率 → 错误
	BlockingThreshold = 0.95 // 95% 使用率 → 硬性阻断
)

// TokenWarningState 根据上下文窗口对当前 token 使用量进行分级。
type TokenWarningState int

const (
	// TokenStateOK 使用量在安全范围内。
	TokenStateOK TokenWarningState = iota
	// TokenStateWarning 接近上下文限制。
	TokenStateWarning
	// TokenStateError 非常接近上下文限制。
	TokenStateError
	// TokenStateBlocking 已达硬性限制，必须压缩或拒绝。
	TokenStateBlocking
)

// String 返回警告状态名称。
func (s TokenWarningState) String() string {
	switch s {
	case TokenStateOK:
		return "ok"
	case TokenStateWarning:
		return "warning"
	case TokenStateError:
		return "error"
	case TokenStateBlocking:
		return "blocking"
	default:
		return "unknown"
	}
}

// EstimateContentBlockTokens 估算单个内容块的 token 数。
// 不同块类型有不同的 token 密度。
func EstimateContentBlockTokens(block ContentBlock) int {
	switch block.Type {
	case "text":
		if len(block.Text) == 0 {
			return 0
		}
		return max(1, len(block.Text)/CharsPerToken)

	case "tool_use":
		// tool_use 块包含 JSON 输入 — 使用 JSON 密度比率。
		tokens := len(block.ToolName)/CharsPerToken + 5 // 工具名 + 帧开销
		if len(block.Input) > 0 {
			tokens += len(block.Input) / JSONCharsPerToken
		}
		return max(1, tokens)

	case "tool_result":
		if len(block.Text) == 0 {
			return 1
		}
		return max(1, len(block.Text)/CharsPerToken)

	case "image":
		return ImageTokenEstimate

	case "document":
		return DocumentTokenEstimate

	case "thinking":
		if len(block.Text) == 0 {
			return 0
		}
		return max(1, len(block.Text)/ThinkingCharsPerToken)

	default:
		// 未知块类型 — 使用文本估算作为回退。
		if len(block.Text) > 0 {
			return len(block.Text) / CharsPerToken
		}
		return 1
	}
}

// EstimateMessageTokens 估算单条消息的 token 数，
// 包含每条消息的帧开销。不应用填充因子。
func EstimateMessageTokens(msg Message) int {
	// 如有缓存的估算值则直接使用。
	if msg.TokenEstimate > 0 {
		return msg.TokenEstimate
	}

	total := MessageOverhead
	for _, block := range msg.Content {
		total += EstimateContentBlockTokens(block)
	}
	return total
}

// EstimateMessagesTokens 估算消息切片的总 token 数，
// 包含每条消息的帧开销和 4/3 安全填充因子。
func EstimateMessagesTokens(msgs []Message) int {
	total := 0
	for _, msg := range msgs {
		total += EstimateMessageTokens(msg)
	}
	// 应用 4/3 填充因子防止低估。
	return total * PaddingNumerator / PaddingDenominator
}

// EffectiveContextWindow 计算可用上下文窗口，
// 从总窗口中减去压缩摘要输出的预留空间。
// 对齐 Claude Code 的 getEffectiveContextWindowSize()。
func EffectiveContextWindow(contextWindow, maxOutputTokens int) int {
	effective := contextWindow - ReservedSummaryTokens
	if maxOutputTokens > 0 && maxOutputTokens < effective {
		// 同时为模型输出预留空间。
		effective -= maxOutputTokens
	}
	if effective < 1000 {
		effective = 1000 // 最低下限
	}
	return effective
}

// CalculateTokenWarningState 根据当前 token 使用量和有效上下文窗口大小返回警告状态。
func CalculateTokenWarningState(currentTokens, effectiveWindow int) TokenWarningState {
	if effectiveWindow <= 0 {
		return TokenStateBlocking
	}
	ratio := float64(currentTokens) / float64(effectiveWindow)
	switch {
	case ratio >= BlockingThreshold:
		return TokenStateBlocking
	case ratio >= ErrorThreshold:
		return TokenStateError
	case ratio >= WarningThreshold:
		return TokenStateWarning
	default:
		return TokenStateOK
	}
}

// AutoCompactThreshold 返回触发自动压缩的 token 阈值。
// 等于有效窗口减去 13k 缓冲区，对齐 Claude Code 的逻辑。
func AutoCompactThreshold(effectiveWindow int) int {
	threshold := effectiveWindow - 13_000
	if threshold < 1000 {
		threshold = 1000
	}
	return threshold
}

// max 返回两个整数中的较大值。（Go 1.22 有内置 max，此处显式定义。）
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
