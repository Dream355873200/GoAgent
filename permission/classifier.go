// Package permission 实现权限系统。
package permission

import (
	"context"
)

// AIClassifierConfig 配置 AI 权限分类器。
type AIClassifierConfig struct {
	// Model 用于分类的模型名称。
	Model string
	// Threshold 信任阈值（0-1）。
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
// 通用壳，供用户自定义分类逻辑。
// 框架内置的 LLM YOLO 分类器见 YoloClassifier。
type AIClassifier struct {
	config     AIClassifierConfig
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
