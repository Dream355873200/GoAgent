// Package compaction 实现四层上下文压缩系统。
//
// 对齐 Claude Code 的 compactConversation 流程：
//
//  1. 尝试 SessionMemory 压缩（无自定义指令时）
//  2. 微压缩（先减少 token 再摘要）
//  3. 调用 LLM 生成对话摘要
//
// Prompt 模板对齐 Claude Code 的 src/services/compact/prompt.ts。
package compaction

import (
	"fmt"
	"os"
	"regexp"

	"github.com/Dream355873200/GoAgent/prompts"
)

// CompactMode 决定使用哪种摘要模式。
type CompactMode int

const (
	// CompactModeBase 完整摘要整个对话。
	CompactModeBase CompactMode = iota
	// CompactModePartial 仅摘要最近的消息。
	CompactModePartial
	// CompactModePartialUpTo 摘要+继续工作上下文。
	CompactModePartialUpTo
)

const (
	// NO_TOOLS_PREAMBLE 强制模型只输出文本，不调用工具。
	NO_TOOLS_PREAMBLE = `CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.

- Do NOT use Read, Bash, Grep, Glob, Edit, Write, or ANY other tool.
- You already have all the context you need in the conversation above.
- Tool calls will be REJECTED and will waste your only turn — you will fail the task.
- Your entire response must be plain text: an <analysis> block followed by a <summary> block.

`

	// NO_TOOLS_TRAILER 提醒模型不要调用工具。
	NO_TOOLS_TRAILER = "\n\nREMINDER: Do NOT call any tools. Respond with plain text only — " +
		"an <analysis> block followed by a <summary> block. " +
		"Tool calls will be rejected and you will fail the task."

	// DETAILED_ANALYSIS_INSTRUCTION_BASE 完整对话的摘要分析指令。
	DETAILED_ANALYSIS_INSTRUCTION_BASE = `Before providing your final summary, wrap your analysis in <analysis> tags to organize your thoughts and ensure you've covered all necessary points. In your analysis process:

1. Chronologically analyze each message and section of the conversation. For each section thoroughly identify:
   - The user's explicit requests and intents
   - Your approach to addressing the user's requests
   - Key decisions, technical concepts and code patterns
   - Specific details like:
     - file names
     - full code snippets
     - function signatures
     - file edits
   - Errors that you ran into and how you fixed them
   - Pay special attention to specific user feedback that you received, especially if the user told you to do something differently.
2. Double-check for technical accuracy and completeness, addressing each required element thoroughly.`

	// DETAILED_ANALYSIS_INSTRUCTION_PARTIAL 最近消息的摘要分析指令。
	DETAILED_ANALYSIS_INSTRUCTION_PARTIAL = `Before providing your final summary, wrap your analysis in <analysis> tags to organize your thoughts and ensure you've covered all necessary points. In your analysis process:

1. Analyze the recent messages chronologically. For each section thoroughly identify:
   - The user's explicit requests and intents
   - Your approach to addressing the user's requests
   - Key decisions, technical concepts and code patterns
   - Specific details like:
     - file names
     - full code snippets
     - function signatures
     - file edits
   - Errors that you ran into and how you fixed them
   - Pay special attention to specific user feedback that you received, especially if the user told you to do something differently.
2. Double-check for technical accuracy and completeness, addressing each required element thoroughly.`

	// BASE_COMPACT_PROMPT 完整对话摘要的 prompt。
	BASE_COMPACT_PROMPT = `Your task is to create a detailed summary of the conversation so far, paying close attention to the user's explicit requests and your previous actions.
This summary should be thorough in capturing technical details, code patterns, and architectural decisions that would be essential for continuing development work without losing context.

` + DETAILED_ANALYSIS_INSTRUCTION_BASE + `

Your summary should include the following sections:

1. Primary Request and Intent: Capture all of the user's explicit requests and intents in detail
2. Key Technical Concepts: List all important technical concepts, technologies, and frameworks discussed.
3. Files and Code Sections: Enumerate specific files and code sections examined, modified, or created. Pay special attention to the most recent messages and include full code snippets where applicable and include a summary of why this file read or edit is important.
4. Errors and fixes: List all errors that you ran into, and how you fixed them. Pay special attention to specific user feedback that you received, especially if the user told you to do something differently.
5. Problem Solving: Document problems solved and any ongoing troubleshooting efforts.
6. All user messages: List ALL user messages that are not tool results. These are critical for understanding the users' feedback and changing intent.
7. Pending Tasks: Outline any pending tasks that you have explicitly been asked to work on.
8. Current Work: Describe in detail precisely what was being worked on immediately before this summary request, paying special attention to the most recent messages from both user and assistant. Include file names and code snippets where applicable.
9. Optional Next Step: List the next step that you will take that is related to the most recent work you were doing. IMPORTANT: ensure that this step is DIRECTLY in line with the user's most recent explicit requests, and the task you were working on immediately before this summary request. If your last task was concluded, then only list next steps if they are explicitly in line with the users request. Do not start on tangential requests or really old requests that were already completed without confirming with the user first.
                       If there is a next step, include direct quotes from the most recent conversation showing exactly what task you were working on and where you left off. This should be verbatim to ensure there's no drift in task interpretation.

Here's an example of how your output should be structured:

<example>
<analysis>
[Your thought process, ensuring all points are covered thoroughly and accurately]
</analysis>

<summary>
1. Primary Request and Intent:
   [Detailed description]

2. Key Technical Concepts:
   - [Concept 1]
   - [Concept 2]
   - [...]

3. Files and Code Sections:
   - [File Name 1]
      - [Summary of why this file is important]
      - [Summary of the changes made to this file, if any]
      - [Important Code Snippet]
   - [File Name 2]
      - [Important Code Snippet]
   - [...]

4. Errors and fixes:
    - [Detailed description of error 1]:
      - [How you fixed the error]
      - [User feedback on the error if any]
    - [...]

5. Problem Solving:
    [Description of solved problems and ongoing troubleshooting]

6. All user messages:
    - [Detailed non tool use user message]
    - [...]

7. Pending Tasks:
    - [Task 1]
    - [Task 2]
    - [...]

8. Current Work:
    [Precise description of current work]

9. Optional Next Step:
    [Optional Next step to take]

</summary>
</example>

Please provide your summary based on the conversation so far, following this structure and ensuring precision and thoroughness in your response.

There may be additional summarization instructions provided in the included context. If so, remember to follow these instructions when creating the above summary. Examples of instructions include:
<example>
## Compact Instructions
When summarizing the conversation focus on typescript code changes and also remember the mistakes you made and how you fixed them.
</example>

<example>
# Summary instructions
When you are using compact - please focus on test output and code changes. Include file reads verbatim.
</example>`

	// PARTIAL_COMPACT_PROMPT 部分摘要（仅最近消息）的 prompt。
	PARTIAL_COMPACT_PROMPT = `Your task is to create a detailed summary of the RECENT portion of the conversation — the messages that follow earlier retained context. The earlier messages are being kept intact and do NOT need to be summarized. Focus your summary on what was discussed, learned, and accomplished in the recent messages only.

` + DETAILED_ANALYSIS_INSTRUCTION_PARTIAL + `

Your summary should include the following sections:

1. Primary Request and Intent: Capture the user's explicit requests and intents from the recent messages
2. Key Technical Concepts: List important technical concepts, technologies, and frameworks discussed recently.
3. Files and Code Sections: Enumerate specific files and code sections examined, modified, or created. Include full code snippets where applicable and include a summary of why this file read or edit is important.
4. Errors and fixes: List errors encountered and how they were fixed.
5. Problem Solving: Document problems solved and any ongoing troubleshooting efforts.
6. All user messages: List ALL user messages from the recent portion that are not tool results.
7. Pending Tasks: Outline any pending tasks from the recent messages.
8. Current Work: Describe precisely what was being worked on immediately before this summary request.
9. Optional Next Step: List the next step related to the most recent work. Include direct quotes from the most recent conversation.

Here's an example of how your output should be structured:

<example>
<analysis>
[Your thought process, ensuring all points are covered thoroughly and accurately]
</analysis>

<summary>
1. Primary Request and Intent:
   [Detailed description]

2. Key Technical Concepts:
   - [Concept 1]
   - [Concept 2]

3. Files and Code Sections:
   - [File Name 1]
      - [Summary of why this file is important]
      - [Important Code Snippet]

4. Errors and fixes:
    - [Error description]:
      - [How you fixed it]

5. Problem Solving:
    [Description]

6. All user messages:
    - [Detailed non tool use user message]

7. Pending Tasks:
    - [Task 1]

8. Current Work:
    [Precise description of current work]

9. Optional Next Step:
    [Optional Next step to take]

</summary>
</example>

Please provide your summary based on the RECENT messages only (after the retained earlier context), following this structure and ensuring precision and thoroughness in your response.`

	// PARTIAL_COMPACT_UP_TO_PROMPT 摘要+继续工作上下文的 prompt。
	PARTIAL_COMPACT_UP_TO_PROMPT = `Your task is to create a detailed summary of this conversation. This summary will be placed at the start of a continuing session; newer messages that build on this context will follow after your summary (you do not see them here). Summarize thoroughly so that someone reading only your summary and then the newer messages can fully understand what happened and continue the work.

` + DETAILED_ANALYSIS_INSTRUCTION_BASE + `

Your summary should include the following sections:

1. Primary Request and Intent: Capture the user's explicit requests and intents in detail
2. Key Technical Concepts: List important technical concepts, technologies, and frameworks discussed.
3. Files and Code Sections: Enumerate specific files and code sections examined, modified, or created. Include full code snippets where applicable and include a summary of why this file read or edit is important.
4. Errors and fixes: List errors encountered and how they were fixed.
5. Problem Solving: Document problems solved and any ongoing troubleshooting efforts.
6. All user messages: List ALL user messages that are not tool results.
7. Pending Tasks: Outline any pending tasks.
8. Work Completed: Describe what was accomplished by the end of this portion.
9. Context for Continuing Work: Summarize any context, decisions, or state that would be needed to understand and continue the work in subsequent messages.

Here's an example of how your output should be structured:

<example>
<analysis>
[Your thought process, ensuring all points are covered thoroughly and accurately]
</analysis>

<summary>
1. Primary Request and Intent:
   [Detailed description]

2. Key Technical Concepts:
   - [Concept 1]
   - [Concept 2]

3. Files and Code Sections:
   - [File Name 1]
      - [Summary of why this file is important]
      - [Important Code Snippet]

4. Errors and fixes:
    - [Error description]:
      - [How you fixed it]

5. Problem Solving:
    [Description]

6. All user messages:
    - [Detailed non tool use user message]

7. Pending Tasks:
    - [Task 1]

8. Work Completed:
    [Description of what was accomplished]

9. Context for Continuing Work:
    [Key context, decisions, or state needed to continue the work]

</summary>
</example>

Please provide your summary following this structure, ensuring precision and thoroughness in your response.`
)

// GetCompactPrompt 返回对齐 Claude Code 的摘要 prompt。
// promptFile 非空时优先从外部文件加载，否则使用嵌入的默认值。
func GetCompactPrompt(customInstructions string, mode CompactMode, promptFile string) string {
	var template string
	if promptFile != "" {
		if data, err := os.ReadFile(promptFile); err == nil {
			template = string(data)
		} else {
			template = prompts.MustLoad(prompts.Compact)
		}
	} else {
		template = prompts.MustLoad(prompts.Compact)
	}

	prompt := NO_TOOLS_PREAMBLE + template

	if customInstructions != "" {
		prompt += fmt.Sprintf("\n\nAdditional Instructions:\n%s", customInstructions)
	}

	prompt += NO_TOOLS_TRAILER

	return prompt
}

// GetSystemCompactPrompt 返回用于摘要生成的系统提示词。
func GetSystemCompactPrompt() string {
	// 压缩时使用的简单系统提示词
	return "你是一个有用的 AI 助手，负责摘要对话内容。"
}

// FormatCompactSummary 格式化 LLM 返回的摘要。
// 去掉 <analysis> 标签，将 <summary> 标签替换为可读的章节标题。
func FormatCompactSummary(raw string) string {
	formatted := raw

	// 去掉 analysis 部分（它是起草时的草稿）。
	formatted = stripXMLSection(formatted, "analysis")

	// 提取并格式化 summary 部分。
	formatted = formatXMLSection(formatted, "summary", "Summary:\n")

	// 清理多余的空行。
	formatted = compressWhitespace(formatted)

	return formatted
}

// stripXMLSection 移除指定名称的 XML 标签及其内容。
func stripXMLSection(s, tagName string) string {
	pattern := fmt.Sprintf(`<%s>[\s\S]*?</%s>`, tagName, tagName)
	re := regexp.MustCompile(pattern)
	return re.ReplaceAllString(s, "")
}

// formatXMLSection 将指定名称的 XML 标签替换为格式化文本。
func formatXMLSection(s, tagName, prefix string) string {
	pattern := fmt.Sprintf(`<%s>([\s\S]*?)</%s>`, tagName, tagName)
	re := regexp.MustCompile(pattern)
	return re.ReplaceAllString(s, prefix+"$1")
}

// compressWhitespace 将多个连续空行压缩为单个。
func compressWhitespace(s string) string {
	re := regexp.MustCompile(`\n\n+`)
	return re.ReplaceAllString(s, "\n\n")
}
