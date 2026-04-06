package permission

import "github.com/Dream355873200/GoAgent/prompts"

const (
	// YoloPromptFile 是 YOLO 分类器 system prompt 的文件名。
	YoloPromptFile = "yolo-classifier.prompt.md"
)

// YoloSystemPrompt 返回 YOLO 分类器的 system prompt。
// 从 prompts 包加载，对齐 Claude Code auto_mode_system_prompt.txt + permissions_external.txt。
func YoloSystemPrompt() string {
	return prompts.MustLoad(YoloPromptFile)
}

// stage1Suffix 是 Stage 1（快速分类）追加到 system prompt 的后缀。
const stage1Suffix = "\nErr on the side of blocking. <block> immediately."

// stage2Suffix 是 Stage 2（思考分类）追加到 system prompt 的后缀。
const stage2Suffix = "\nReview the classification process and follow it carefully, making sure you deny actions that should be blocked. As a reminder, explicit (not suggestive or implicit) user confirmation is required to override blocks. Use <thinking> before responding with <block>."
