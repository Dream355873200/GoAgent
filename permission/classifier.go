// Package permission 实现权限系统。
package permission

import (
	"context"
	"strings"
)

// AIClassifierConfig 配置 AI 权限分类器。
type AIClassifierConfig struct {
	// Model 用于分类的模型名称。
	Model string
	//Threshold 信任阈值（0-1）。
	Threshold float64
}

// DefaultAIClassifierConfig 返回默认配置。
func DefaultAIClassifierConfig() AIClassifierConfig {
	return AIClassifierConfig{
		Model:     "claude-sonnet-4-6-v1",
		Threshold: 0.8,
	}
}

// ClassificationResult 是 AI 分类结果。
type ClassificationResult struct {
	// Behavior 预测的行为：allow, ask, deny。
	Behavior string
	// Confidence 置信度（0-1）。
	Confidence float64
	// Reason 分类理由。
	Reason string
}

// AIClassifier 使用 AI 模型进行权限分类。
// 对齐 Claude Code 的 bashClassifier/yoloClassifier 机制。
type AIClassifier struct {
	config AIClassifierConfig
	// classifier 是实际的分类函数
	classifier func(ctx context.Context, toolName, input string) (*ClassificationResult, error)
}

// NewAIClassifier 创建 AI 权限分类器。
func NewAIClassifier(config AIClassifierConfig, classifyFn func(ctx context.Context, toolName, input string) (*ClassificationResult, error)) *AIClassifier {
	if config.Model == "" {
		config = DefaultAIClassifierConfig()
	}
	return &AIClassifier{
		config:     config,
		classifier: classifyFn,
	}
}

// Classify 对工具调用进行分类。
func (c *AIClassifier) Classify(ctx context.Context, toolName, input string) (string, error) {
	if c.classifier == nil {
		return "ask", nil // 默认询问
	}

	result, err := c.classifier(ctx, toolName, input)
	if err != nil {
		return "ask", err
	}

	if result.Confidence < c.config.Threshold {
		return "ask", nil // 置信度不足，询问用户
	}

	return result.Behavior, nil
}

// BashClassifier 专门分类 Bash 命令的分类器。
type BashClassifier struct {
	classifier func(cmd string) (string, float64)
}

// NewBashClassifier 创建 Bash 命令分类器。
func NewBashClassifier(classifyFn func(cmd string) (string, float64)) *BashClassifier {
	return &BashClassifier{classifier: classifyFn}
}

// ClassifyCommand 分类 Bash 命令。
// 返回: behavior (allow/ask/deny), confidence (0-1)
func (c *BashClassifier) ClassifyCommand(cmd string) (string, float64) {
	if c.classifier == nil {
		// 默认实现：基于模式匹配
		return classifyBashByPattern(cmd)
	}
	return c.classifier(cmd)
}

// classifyBashByPattern 使用模式匹配分类 Bash 命令。
func classifyBashByPattern(cmd string) (string, float64) {
	cmd = strings.TrimSpace(cmd)

	// 高危命令：直接拒绝
	highDanger := []string{
		"rm -rf /",
		"rm -rf /*",
		"dd if=",
		":(){ :|:& };:", // fork bomb
		"mkfs",
		"dd conv=notrunc",
	}
	for _, pattern := range highDanger {
		if strings.HasPrefix(cmd, pattern) {
			return "deny", 0.99
		}
	}

	// 中危命令：询问
	mediumDanger := []string{
		"rm -r",
		"rm -f",
		"mv ",
		"chmod 777",
		"chmod +x",
		"delet",
		"drop ",
		"truncat",
	}
	for _, pattern := range mediumDanger {
		if strings.Contains(cmd, pattern) {
			return "ask", 0.7
		}
	}

	// 只读/安全命令：允许
	safe := []string{
		"git status",
		"git diff",
		"git log",
		"git show",
		"git branch",
		"git remote",
		"ls",
		"pwd",
		"cat ",
		"head ",
		"tail ",
		"grep ",
		"find ",
		"echo ",
		"which ",
		"whereis ",
		"whoami",
		"date",
		"uptime",
		"df",
		"free",
	}
	for _, pattern := range safe {
		if strings.HasPrefix(cmd, pattern) {
			return "allow", 0.95
		}
	}

	// 默认询问
	return "ask", 0.5
}

// PermissionPredictor 预测权限需求的预测器。
type PermissionPredictor struct {
	classifiers map[string]*BashClassifier
}

// NewPermissionPredictor 创建权限预测器。
func NewPermissionPredictor() *PermissionPredictor {
	return &PermissionPredictor{
		classifiers: map[string]*BashClassifier{
			"bash": NewBashClassifier(nil),
		},
	}
}

// Predict 预测给定工具和输入的权限需求。
func (p *PermissionPredictor) Predict(toolName, input string) (string, float64) {
	if clf, ok := p.classifiers[toolName]; ok && toolName == "Bash" {
		return clf.ClassifyCommand(input)
	}
	// 默认：询问
	return "ask", 0.5
}

// RegisterClassifier 为特定工具注册分类器。
func (p *PermissionPredictor) RegisterClassifier(toolName string, clf *BashClassifier) {
	p.classifiers[toolName] = clf
}
