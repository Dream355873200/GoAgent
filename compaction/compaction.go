// Package compaction 实现四层上下文压缩系统。
//
// 各层从轻到重：
//
//	Layer 0 (Budget):   将超大工具结果持久化到磁盘，替换为引用
//	Layer 1 (Snip):     截断旧消息的内容，保留结构
//	Layer 2 (Micro):    移除模型已处理的工具结果
//	Layer 3 (Collapse): 将中间轮次折叠为摘要视图（读时投影）
//	Layer 4 (Auto):     调用 Summarizer 对整个对话进行摘要
//
// 此外，Reactive Compact 在 413 错误发生后进行事后处理。
//
// 本包可独立使用 — 提供任意 Summarizer 实现（或 nil 禁用 Layer 4），
// 即可接入你自己的 agent 循环。
package compaction

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Dream355873200/GoAgent/message"
	"github.com/Dream355873200/GoAgent/provider"
)

// Summarizer 是一个文本摘要函数。它将本包与特定 LLM 提供者解耦 —
// 调用者可以将任何 LLM、本地模型、甚至静态摘要器包装在此签名背后。
//
// 示例：
//
//	summarizer := func(ctx context.Context, text string) (string, error) {
//	    return myLLM.Summarize(ctx, text)
//	}
type Summarizer func(ctx context.Context, text string) (string, error)

// Layer 表示压缩层编号。
type Layer int

const (
	LayerBudget   Layer = 0 // L0: 超大 tool result 截断
	LayerSnip     Layer = 1 // L1: 旧消息文本截断
	LayerMicro    Layer = 2 // L2: 移除已消费的 tool result
	LayerCollapse Layer = 3 // L3: 旧轮次折叠
	LayerAuto     Layer = 4 // L4: LLM 摘要
)

// Config 控制压缩行为。
type Config struct {
	// Summarizer 是 Layer 4（自动摘要）的可选函数。
	// 如果为 nil，Layer 4 将被禁用。
	Summarizer Summarizer

	// AutoCompactThreshold 是触发自动压缩的上下文窗口比例（0.0-1.0）。
	// 默认值：0.8
	AutoCompactThreshold float64

	// MaxResultSize 是单个工具结果在持久化到磁盘前的最大字符数。
	// 默认值：50000
	MaxResultSize int

	// MicroStripThreshold 是允许 L2 micro 层剥离"已消费工具结果"的
	// 上下文窗口比例（0.0-1.0）。只有当估算 token 超过
	// contextWindow * MicroStripThreshold 时才执行剥离；
	// 未达阈值时不剥离（避免每轮剥离导致模型失忆、反复读取同一批
	// 文件原地打转）。默认值：0.5；设为负数可完全禁用剥离。
	MicroStripThreshold float64

	// TailBudgetRatio 是 L1/L3"保护尾部"的 token 预算比例（相对
	// contextWindow）：折叠与截断只作用于预算之外的旧消息，预算内的
	// 近期原文原样保留。0（默认）= 窗口的 10%；负数 = 关闭预算，按
	// 固定条数保护（折叠保护最后 6 条、截断保护最后 4 条，v0.14 及
	// 之前的行为）。保护条数在任何模式下都不低于对应下限。
	//
	// 为什么要按比例：固定条数是按 200k 窗口校准的——百万上下文的
	// 模型上 6 条原文只占窗口的 ~1%，"长期不保护、一旦折叠只保 6 条"
	// 的悬崖在长上下文下不可接受。按比例让保护随窗口自动缩放。
	TailBudgetRatio float64

	// Layers 指定启用的压缩层（可选）。
	// nil 或空 = 启用全部层（向后兼容）。
	// 例如：[]Layer{LayerBudget, LayerMicro, LayerAuto} 只启用 L0+L2+L4。
	Layers []Layer

	// PromptFile 外部 compact prompt 文件路径（空则用嵌入默认值）。
	PromptFile string

	// PostCompact 压缩后重注入源（可选）。每次实际压缩（Layer 4 摘要
	// 成功 / 413 响应式压缩成功）完成后，逐个调用 Reminders，返回的
	// 文本各自独立成 user 消息追加在压缩边界之后——重新注入被摘要
	// 吞掉的关键上下文（已读文件、宿主固定资产等）。见 reminder.go。
	PostCompact []ReminderSource
}

// Manager 协调压缩层。
type Manager struct {
	config        Config
	provider      provider.Provider
	enabledLayers map[Layer]bool
	budget        *budgetCompressor
	snip          *snipCompressor
	micro         *microCompressor
	collapse      *collapseCompressor
	auto          *autoCompressor
	tracking      AutoCompactTracking
}

// summarizeTimeout 是一次自动摘要模型调用的超时。
const summarizeTimeout = 2 * time.Minute

// SetProvider 设置用于 LLM 摘要的提供者。未通过 Config.Summarizer 自定义
// 摘要函数时，Layer 4 自动摘要改用该提供者（此前 Layer 4 从未接上摘要器，
// 实际不会生效）。
func (m *Manager) SetProvider(p provider.Provider) {
	m.provider = p
	if m.config.Summarizer != nil || p == nil {
		return
	}
	m.auto.summarizer = func(ctx context.Context, prompt string) (string, error) {
		cctx, cancel := context.WithTimeout(ctx, summarizeTimeout)
		defer cancel()
		resp, err := p.Complete(cctx, &provider.Request{
			Messages:  []message.Message{message.NewUserMessage(prompt)},
			MaxTokens: 8192,
		})
		if err != nil {
			return "", err
		}
		text := strings.TrimSpace(message.ExtractText(resp.Message))
		if text == "" {
			return "", fmt.Errorf("摘要为空")
		}
		return text, nil
	}
}

// injectPostCompactReminders 调用全部重注入源，把提醒追加为 user 消息。
// 内置一条继续重定向：压缩把执行证据折叠成摘要后，模型最容易在边界处
// 误判「任务已完成」而提前收轮（stop=end_turn），每轮压缩后显式纠正一次。
func (m *Manager) injectPostCompactReminders(ctx context.Context, msgs []message.Message) []message.Message {
	msgs = appendReminderMessages(msgs, []string{
		"以上对话已被压缩成摘要，部分执行细节可能不在上下文里。" +
			"请继续完成摘要中「未完成事项 / 下一步」列出的工作；" +
			"不要因为上下文被压缩而重新开始、缩水交付或宣告完成。" +
			"若不确定当前进度，先用工具核实（读文件 / 查状态 / 看画面）再继续。",
	})
	for _, src := range m.config.PostCompact {
		if src == nil {
			continue
		}
		msgs = appendReminderMessages(msgs, src.Reminders(ctx))
	}
	return msgs
}

// Compact 执行完整压缩流程，对齐 Claude Code 的 compactConversation。
// 如果 Provider 未设置，返回错误。
func (m *Manager) Compact(ctx context.Context, messages []message.Message, customInstructions string, isAutoCompact bool) (*CompactionResult, error) {
	if m.provider == nil {
		return nil, fmt.Errorf("Compact 需要 Provider")
	}

	cfg := CompactConfig{
		Provider:           m.provider,
		CustomInstructions: customInstructions,
		IsAutoCompact:      isAutoCompact,
		ContextWindow:      200_000, // TODO: 从 provider capabilities 获取
		Threshold:          int(float64(200_000) * m.config.AutoCompactThreshold),
		MicrocompactCfg: MicrocompactConfig{
			MaxToolResultChars: m.config.MaxResultSize,
			ProtectedTail:      4,
		},
		PromptFile: m.config.PromptFile,
	}

	return Compact(ctx, messages, cfg)
}

// AutoCompactTracking 追踪自动压缩状态，用于熔断器逻辑。
// 对齐 Claude Code 的 AutoCompactTrackingState。
type AutoCompactTracking struct {
	// ConsecutiveFailures 计数连续自动压缩失败次数。
	// 超过 MaxConsecutiveAutocompactFailures 后，自动压缩将被抑制。
	ConsecutiveFailures int

	// LastCompactedTurn 是上次成功压缩时的轮次编号。
	LastCompactedTurn int

	// TurnsSinceCompact 计数自上次成功压缩以来的轮次数。
	TurnsSinceCompact int

	// Compacted 标记当前迭代是否发生了压缩。
	Compacted bool
}

// MaxConsecutiveAutocompactFailures 是熔断器阈值。
// 超过此数量的连续失败后，自动压缩将被抑制以防止无限重试循环。
// 对齐 Claude Code 的 MAX_CONSECUTIVE_AUTOCOMPACT_FAILURES。
const MaxConsecutiveAutocompactFailures = 3

// NewManager 创建一个新的压缩管理器。
func NewManager(cfg Config) *Manager {
	if cfg.AutoCompactThreshold == 0 {
		cfg.AutoCompactThreshold = 0.8
	}
	if cfg.MaxResultSize == 0 {
		cfg.MaxResultSize = 50_000
	}
	if cfg.MicroStripThreshold == 0 {
		cfg.MicroStripThreshold = 0.5
	}
	if cfg.TailBudgetRatio == 0 {
		cfg.TailBudgetRatio = 0.1
	}

	// 构建启用层映射。nil/空 = 全部启用（向后兼容）。
	enabled := make(map[Layer]bool)
	if len(cfg.Layers) == 0 {
		for _, l := range []Layer{LayerBudget, LayerSnip, LayerMicro, LayerCollapse, LayerAuto} {
			enabled[l] = true
		}
	} else {
		for _, l := range cfg.Layers {
			enabled[l] = true
		}
	}

	return &Manager{
		config:        cfg,
		enabledLayers: enabled,
		budget:        &budgetCompressor{maxSize: cfg.MaxResultSize},
		snip:          &snipCompressor{tailBudgetRatio: cfg.TailBudgetRatio, protectTail: 4},
		micro:         &microCompressor{},
		collapse:      &collapseCompressor{tailBudgetRatio: cfg.TailBudgetRatio, protectTail: 6},
		auto:          &autoCompressor{summarizer: cfg.Summarizer, threshold: cfg.AutoCompactThreshold},
	}
}

// CircuitBreakerTripped 检查熔断器是否已触发。
// 连续失败次数超过阈值时返回 true。
func (m *Manager) CircuitBreakerTripped() bool {
	return m.tracking.ConsecutiveFailures >= MaxConsecutiveAutocompactFailures
}

// RecordAutocompactResult 记录一次自动压缩的结果，更新熔断器状态。
func (m *Manager) RecordAutocompactResult(success bool, currentTurn int) {
	if success {
		m.tracking.ConsecutiveFailures = 0
		m.tracking.LastCompactedTurn = currentTurn
		m.tracking.TurnsSinceCompact = 0
		m.tracking.Compacted = true
	} else {
		m.tracking.ConsecutiveFailures++
		m.tracking.Compacted = false
	}
}

// IncrementTurn 在每轮结束时调用，增加 TurnsSinceCompact 计数。
func (m *Manager) IncrementTurn() {
	m.tracking.TurnsSinceCompact++
	m.tracking.Compacted = false
}

// Tracking 返回当前的自动压缩追踪状态（只读副本）。
func (m *Manager) Tracking() AutoCompactTracking {
	return m.tracking
}

// Apply 按顺序在消息上运行启用的压缩层。
// 返回处理后的消息和所有层释放的总 token 数。
func (m *Manager) Apply(ctx context.Context, messages []message.Message, contextWindow int) ([]message.Message, int) {
	totalFreed := 0
	var freed int

	// Layer 0: Budget — 截断超大工具结果。
	// TODO: 实现真正的磁盘持久化 + 引用替换（当前仅截断保留头尾）。
	if m.enabledLayers[LayerBudget] {
		messages, freed = m.budget.apply(messages)
		totalFreed += freed
	}

	// Layer 2: Micro — 截断超长工具结果；仅在上下文达到压力阈值时
	// 剥离已消费的工具结果（对齐 Claude Code：接近窗口上限才剥离，
	// 而非每轮剥离，否则模型会因失忆反复读取同一批文件原地打转）。
	if m.enabledLayers[LayerMicro] {
		messages, freed = m.micro.apply(messages, contextWindow, m.config.MicroStripThreshold)
		totalFreed += freed
	}

	// Layer 4: Auto — AI 辅助压缩，主路径：估算占用达到阈值且熔断未触发
	// 时，直接调模型做语义摘要（上下文质量优于确定性截断/折叠）。
	threshold := int(float64(contextWindow) * m.config.AutoCompactThreshold)
	summaryOK := false
	if !m.enabledLayers[LayerAuto] || m.CircuitBreakerTripped() {
		// 未启用 L4 或熔断已触发：直接走确定性兜底。
	} else if currentTokens := estimateMessagesTokens(messages); currentTokens <= threshold {
		summaryOK = true // 未达压缩阈值：本轮无需任何压缩
	} else {
		compacted, err := m.auto.apply(ctx, messages, contextWindow)
		if err == nil {
			after := estimateMessagesTokens(compacted)
			freed = currentTokens - after
			totalFreed += freed
			compacted = m.injectPostCompactReminders(ctx, compacted)
			messages = compacted
			// 摘要后仍超过阈值（例如保留的尾部本身就很大）视为失败：继续
			// 每轮重新摘要只会烧钱，连续失败交给熔断器止损。
			afterOK := after <= threshold
			m.RecordAutocompactResult(afterOK, 0)
			summaryOK = afterOK
		} else {
			m.RecordAutocompactResult(false, 0)
		}
	}
	if summaryOK {
		return messages, totalFreed
	}

	// Layer 1 + Layer 3: 确定性截断/折叠——降级为兜底：仅在未启用 L4、
	// 摘要调用失败、或熔断已触发（模型路径当前不可用）时执行。
	if m.enabledLayers[LayerSnip] {
		messages, freed = m.snip.apply(messages, contextWindow)
		totalFreed += freed
	}
	if m.enabledLayers[LayerCollapse] {
		messages, freed = m.collapse.apply(messages, contextWindow)
		totalFreed += freed
	}

	return messages, totalFreed
}

// HandleOverflow 尝试从 413 prompt-too-long 错误中恢复。
// 返回恢复后的消息和恢复是否成功。
func (m *Manager) HandleOverflow(ctx context.Context, messages []message.Message, contextWindow int) ([]message.Message, bool) {
	// 第一次尝试：折叠排空。
	drained, freed := m.collapse.drain(messages)
	if freed > 0 {
		return drained, true
	}

	// 第二次尝试：响应式压缩（基于模型）。
	compacted, err := m.auto.apply(ctx, messages, contextWindow)
	if err == nil {
		return m.injectPostCompactReminders(ctx, compacted), true
	}

	return messages, false
}

// --- Layer 0: Budget ---

// 图片回收共享策略（与 micro_compact.go 的图片处理保持一致）：
// 含内联图片的结果绝不允许 head+tail 截断——截断会产出损坏 base64，发给
// 端点必 400 且会话永久卡死。最近 imgTailMessages 条消息内的图片完整保留
// （一轮可含多张参考图），更早的整体回收为占位符——占位符带上正文里给出
// 的原图路径，模型之后可以重新读取。
const imgTailMessages = 40

// imagePathRe 匹配图片结果正文中的绝对路径（Windows 盘符路径或 POSIX 路径），
// 路径前须是行首、空白或常见分隔符。
var imagePathRe = regexp.MustCompile(`(?:^|[\s(（:：=「"'])((?:[A-Za-z]:[\\/]|/)[^\s"'，。；、)）\]」]+)`)

// imagePlaceholder 生成图片结果被回收后的占位文本。路径只在剥掉图片标记后的
// 正文里查找：base64 数据本身含有 "/"。
func imagePlaceholder(text string) string {
	if m := imagePathRe.FindStringSubmatch(stripImageMarkers(text)); m != nil {
		return "[图片已省略：历史图片已回收以节省上下文，原图位于 " + m[1] + "；需要时可重新读取该文件]"
	}
	return "[图片已省略：历史图片已回收以节省上下文；需要时请重新读取原图]"
}

// recycleImageResult 判定并生成图片结果回收文本（调用方须先确认含图片标记）。
// 返回 (替换文本, true) = 应回收；(空, false) = 保留完整原样——不是"可以截断"。
func recycleImageResult(messages []message.Message, i int, text string) (string, bool) {
	if !strings.Contains(text, imageMarker) {
		return "", false
	}
	if i >= len(messages)-imgTailMessages {
		return "", false
	}
	return imagePlaceholder(text), true
}

type budgetCompressor struct {
	maxSize int
}

// cowText 以写时复制的方式改写 result[i].Content[j].Text：每条消息首次被
// 改写前先克隆其内容块。消息是值类型但 Content 切片共享底层数组，就地改写
// 会篡改调用方手里的历史、已持久化的对话记录，并与并发读取者构成数据竞争。
type cowText map[int]bool

func (c cowText) set(result []message.Message, i, j int, text string) {
	if !c[i] {
		result[i] = result[i].CloneContent()
		c[i] = true
	}
	result[i].Content[j].Text = text
}

func (b *budgetCompressor) apply(messages []message.Message) ([]message.Message, int) {
	freed := 0
	cow := cowText{}
	result := make([]message.Message, len(messages))
	copy(result, messages)
	for i, msg := range messages {
		for j, block := range msg.Content {
			if block.Type == "tool_result" && len(block.Text) > b.maxSize {
				original := len(block.Text)
				// 图片豁免：内联图片结果走统一回收策略（禁 head+tail 截断）
				if strings.Contains(block.Text, imageMarker) {
					if repl, ok := recycleImageResult(messages, i, block.Text); ok {
						cow.set(result, i, j, repl)
						freed += (original - len(repl)) / 4
					}
					continue
				}
				text := headTail(block.Text, b.maxSize/2, b.maxSize/2,
					fmt.Sprintf("\n\n[... %d 字符被截断 ...]\n\n", original-b.maxSize))
				cow.set(result, i, j, text)
				freed += (original - len(text)) / 4
			}
		}
	}
	return result, freed
}

// headTail 保留 s 的前 head 字节与后 tail 字节（截断点落在 UTF-8 字符
// 边界），中间以 mark 替代。
func headTail(s string, head, tail int, mark string) string {
	if head+tail >= len(s) {
		return s
	}
	h := head
	for h > 0 && !utf8.RuneStart(s[h]) {
		h--
	}
	t := len(s) - tail
	for t < len(s) && !utf8.RuneStart(s[t]) {
		t++
	}
	return s[:h] + mark + s[t:]
}

// --- Layer 1: Snip ---

type snipCompressor struct {
	tailBudgetRatio float64
	protectTail     int
}

func (s *snipCompressor) apply(messages []message.Message, contextWindow int) ([]message.Message, int) {
	if len(messages) <= 4 {
		return messages, 0 // 消息太少，不需要截断
	}

	// 尾部保护区（预算内 + 下限条数）之外的旧文本才截断；无压力时
	// 保留完整文本，避免每轮截断导致模型丢失先前推理内容、原地打转。
	if estimateMessagesTokens(messages) < int(float64(contextWindow)*0.5) {
		return messages, 0
	}

	cutoff := protectStart(messages, contextWindow, s.tailBudgetRatio, s.protectTail)
	freed := 0

	cow := cowText{}
	result := make([]message.Message, len(messages))
	copy(result, messages)

	for i := 0; i < cutoff; i++ {
		if result[i].Compacted || (i == 0 && result[i].Role == message.RoleUser) {
			continue
		}
		for j, block := range messages[i].Content {
			if block.Type == "text" && len(block.Text) > 200 {
				original := message.EstimateTokens(message.Message{Content: []message.ContentBlock{block}})
				cow.set(result, i, j, headTail(block.Text, 100, 100, "\n[... 已截断 ...]\n"))
				newEstimate := message.EstimateTokens(message.Message{Content: []message.ContentBlock{result[i].Content[j]}})
				freed += original - newEstimate
			}
		}
	}
	return result, freed
}

// --- Layer 2: Micro ---
// 对齐 Claude Code 的 applyToolResultBudget + microcompact：
// 1. 按单条工具结果大小裁剪（>50KB 的替换为截断版）
// 2. 移除已被助手处理过的工具结果内容

// MaxToolResultChars 单条工具结果的最大字符数。
// 超过此阈值的结果会被截断，保留头尾各 10KB。
// 对齐 Claude Code 的 applyToolResultBudget 默认阈值。
const MaxToolResultChars = 50_000

type microCompressor struct{}

func (mc *microCompressor) apply(messages []message.Message, contextWindow int, stripThreshold float64) ([]message.Message, int) {
	freed := 0
	cow := cowText{}

	result := make([]message.Message, len(messages))
	copy(result, messages)

	// Pass 1: 按大小裁剪过长的工具结果（对所有工具结果生效）。
	// 对齐 Claude Code 的 applyToolResultBudget()。
	// 图片豁免：内联图片结果（[IMAGE ...]）绝不能 head+tail 截断——截断会
	// 产出损坏 base64，发给端点必 400 且会话永久卡死。走统一回收策略。
	for i, msg := range messages {
		for j, block := range msg.Content {
			if block.Type == "tool_result" && len(block.Text) > MaxToolResultChars {
				original := len(block.Text)
				if strings.Contains(block.Text, imageMarker) {
					if repl, ok := recycleImageResult(result, i, block.Text); ok {
						cow.set(result, i, j, repl)
						freed += (original - len(repl)) / 4
					}
					continue
				}
				// 保留头尾各 10KB，中间用截断标记替换。
				text := headTail(block.Text, 10_000, 10_000,
					fmt.Sprintf("\n\n[... 中间 %d 字符已截断 ...]\n\n", original-20_000))
				cow.set(result, i, j, text)
				freed += (original - len(text)) / 4
			}
		}
	}

	// Pass 2: 移除已被助手处理过的工具结果内容。
	// 仅在上下文压力达到阈值时执行（threshold<=0 表示禁用剥离）；
	// 无压力时保留完整工具结果，模型跨轮仍能引用先前读到的内容。
	if stripThreshold <= 0 || estimateMessagesTokens(messages) < int(float64(contextWindow)*stripThreshold) {
		return result, freed
	}

	// 追踪哪些 tool_use ID 已被"消费"（后续有助手响应）。
	consumed := make(map[string]bool)

	// 正向扫描：找到已被助手文本跟随的工具结果。
	for i, msg := range messages {
		if msg.Role == message.RoleAssistant {
			// 标记所有前置工具结果为已消费。
			for j := i - 1; j >= 0; j-- {
				for _, block := range messages[j].Content {
					if block.Type == "tool_result" {
						consumed[block.ForToolUseID] = true
					}
				}
				if messages[j].Role == message.RoleAssistant {
					break // 在前一个助手轮次处停止
				}
			}
		}
	}

	// 将已消费的工具结果替换为标记。
	// 图片结果跳过：由 Pass 1 的统一回收策略管理（最近 40 条内完整保留）。
	for i := range result {
		for j, block := range result[i].Content {
			if block.Type == "tool_result" && consumed[block.ForToolUseID] && len(block.Text) > 100 {
				if strings.Contains(block.Text, imageMarker) {
					continue
				}
				original := len(block.Text)
				cow.set(result, i, j, fmt.Sprintf("[内容已处理，移除 %d 字符]", original))
				freed += original / 4
			}
		}
	}
	return result, freed
}

// --- Layer 3: Collapse ---

type collapseCompressor struct {
	tailBudgetRatio float64
	protectTail     int
}

func (c *collapseCompressor) apply(messages []message.Message, contextWindow int) ([]message.Message, int) {
	if len(messages) <= c.protectTail {
		return messages, 0
	}

	// 仅在上下文压力达到阈值时才折叠（与 L1/L2 一致，0.5 门槛）；
	// 注意 drain()（413 溢出恢复）以 contextWindow=0 调用本函数，
	// 此时跳过门槛强制折叠。
	if contextWindow > 0 && estimateMessagesTokens(messages) < int(float64(contextWindow)*0.5) {
		return messages, 0
	}

	// 将保护区之外的旧轮次折叠为摘要消息。保护区 = token 预算内的近期
	// 原文（TailBudgetRatio×contextWindow，随窗口自动缩放），且不少于
	// protectTail 条。切点按 API 轮次对齐：尾部不能以 tool_result 开头
	//（否则与它的 tool_use 被拆开，请求 400）。
	cutoff := roundStart(messages, protectStart(messages, contextWindow, c.tailBudgetRatio, c.protectTail))
	if cutoff <= 0 {
		return messages, 0
	}

	// 已有的折叠摘要原样保留并在其后追加，不再把摘要本身再摘要一遍
	// （反复折叠会让原始任务描述逐渐消失）。
	start := 0
	var summary strings.Builder
	if messages[0].Compacted && messages[0].Role == message.RoleUser {
		summary.WriteString(message.ExtractText(messages[0]))
		summary.WriteString("\n")
		start = 1
	} else {
		summary.WriteString("之前的对话摘要：\n")
	}

	freed := 0
	added := 0
	for i := start; i < cutoff; i++ {
		m := messages[i]
		freed += message.EstimateTokens(m)
		if message.IsToolResultMessage(m) {
			continue
		}
		text := strings.TrimSpace(message.ExtractText(m))
		if text == "" {
			continue
		}
		role := "助手"
		if m.Role == message.RoleUser {
			role = "用户"
			// 第一条用户消息通常是原始任务，多保留一些。
			if i == 0 {
				text = truncateRunes(text, 2000)
			}
		}
		summary.WriteString(fmt.Sprintf("- %s: %s\n", role, truncateRunes(text, 160)))
		added++
	}
	if added == 0 && start == 0 {
		return messages, 0
	}

	summaryMsg := message.Message{
		Role:      message.RoleUser,
		Content:   []message.ContentBlock{{Type: "text", Text: summary.String()}},
		Compacted: true,
	}
	if start == 1 {
		freed += message.EstimateTokens(messages[0])
	}
	freed -= message.EstimateTokens(summaryMsg)

	result := []message.Message{summaryMsg}
	result = append(result, messages[cutoff:]...)
	return result, freed
}

// roundStart 把切点 idx 向前移到 API 轮次边界：只要 messages[idx] 是工具
// 结果消息，就继续前移，直到包含发起这些调用的 assistant 消息。
// protectStart 返回保护区（不截断/不折叠的近期原文）的起始下标：尾部至少
// 保留 protectTail 条消息；配置了正的 tailBudgetRatio 时，保护区按
// "尾部累计估计 token 不超过 contextWindow×ratio" 向更早的方向扩展——
// 窗口越大，原样保留的近期上下文越多。tailBudgetRatio 为负时回到固定
// 条数模式（v0.14 及之前的行为）。
func protectStart(messages []message.Message, contextWindow int, tailBudgetRatio float64, protectTail int) int {
	n := len(messages)
	if n <= protectTail {
		return 0
	}
	floorStart := n - protectTail
	if tailBudgetRatio < 0 || contextWindow <= 0 {
		return floorStart
	}
	budget := int(float64(contextWindow) * tailBudgetRatio)
	used, idx := 0, n-1
	for idx >= 0 && used < budget {
		used += message.EstimateTokens(messages[idx])
		idx--
	}
	start := idx + 1
	if start > floorStart {
		start = floorStart // 预算过小时保底 protectTail 条
	}
	if start < 0 {
		start = 0
	}
	return start
}

func roundStart(messages []message.Message, idx int) int {
	if idx >= len(messages) {
		idx = len(messages) - 1
	}
	for idx > 0 && message.IsToolResultMessage(messages[idx]) {
		idx--
	}
	// 切点落在带工具调用的助手消息上时，把触发它的用户指令一起带上：
	// 尾部从用户输入开始，模型能看到「这轮要干什么」而不只是工具残段。
	// 前一条若是上一轮的 tool_result（本轮没有用户开场）则不跨——跨过去
	// 会把上一轮的孤儿 tool_result 留在尾部，配对即非法。
	if idx > 0 && message.HasToolUse(messages[idx]) && !message.IsToolResultMessage(messages[idx-1]) {
		idx--
	}
	return idx
}

// truncateRunes 按字符截断（不切坏多字节字符）。
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "..."
}

func (c *collapseCompressor) drain(messages []message.Message) ([]message.Message, int) {
	// 激进折叠：折叠除最后 2 条消息外的所有内容。
	return c.apply(messages, 0)
}

// --- Layer 4: Auto ---

type autoCompressor struct {
	summarizer Summarizer
	threshold  float64
}

func (a *autoCompressor) apply(ctx context.Context, messages []message.Message, contextWindow int) ([]message.Message, error) {
	if a.summarizer == nil {
		return messages, fmt.Errorf("自动压缩缺少 summarizer")
	}

	// 构建请求模型摘要的提示。buildConversationText 含工具调用与结果
	// （截断防爆炸）——执行证据链是摘要质量的生命线；图片标记剥离：
	// 内联 base64（150KB+/张）会撑爆摘要输入、截断尾部还会产出损坏数据。
	conversationText := buildConversationText(messages)

	// 对话本身对摘要调用来说太长时，保留开头（原始任务）与结尾（最近进展），
	// 截断中间。
	maxChars := contextWindow * 3 // 粗略估算：1 token ≈ 4 字符，使用窗口的 3/4
	if maxChars > 0 && len(conversationText) > maxChars {
		conversationText = headTail(conversationText, maxChars/5, maxChars-maxChars/5, "\n[... 中间的对话已省略 ...]\n")
	}

	// 提示词要求显式的未完成清单：摘要的读者是一个只拿得到摘要的模型，
	// 「做到哪了/还差什么」是它恢复工作最需要的信息；不写明就会把过程性
	// 叙述误读成已完成，压缩后输出错误内容或提前收轮。
	prompt := "Summarize the following conversation for an AI agent that will continue the work with ONLY this summary as prior context. " +
		"Preserve: the user's original task and requirements, key decisions, file paths, code changes, " +
		"what was executed via tools and how it turned out, open problems, " +
		"and an explicit list of UNFINISHED work / next steps at the end. " +
		"Do not present in-progress work as completed. Be concise.\n\n" +
		conversationText

	summaryText, err := a.summarizer(ctx, prompt)
	if err != nil {
		return messages, fmt.Errorf("自动压缩失败: %w", err)
	}

	// 保留最后 2 条消息以维持上下文连续性；切点按 API 轮次对齐。
	start := roundStart(messages, len(messages)-2)
	if start < 0 {
		start = 0
	}

	// 摘要同时是一条摘要记录：CompactBoundary 指向保留下来的第一条对话记录
	// 消息，循环把它追加进对话记录后，Project 能从记录还原出这个视图。
	summaryMsg := message.Message{
		Role: message.RoleUser,
		Content: []message.ContentBlock{{
			Type: "text",
			Text: "[对话已压缩]\n\n" + summaryText,
		}},
		Compacted:        true,
		IsMeta:           true,
		IsCompactSummary: true,
	}
	for _, m := range messages[start:] {
		if m.UID != "" && !m.IsCompactSummary {
			summaryMsg.CompactBoundary = m.UID
			break
		}
	}

	result := []message.Message{summaryMsg}
	result = append(result, messages[start:]...)
	return result, nil
}

// estimateMessagesTokens 估算消息切片的总 token 数。
func estimateMessagesTokens(messages []message.Message) int {
	total := 0
	for _, msg := range messages {
		total += message.EstimateTokens(msg)
	}
	return total
}
