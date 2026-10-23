package goagent

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// builtin 系工具依赖 builtin 包的 init() 向根包回注 provider（避免 import cycle）。
// 此前宿主若忘了 import builtin，WithBuiltinTools / WithTaskTools / WithBgTaskTools /
// WithAskTools / WithPlanTools / WithIssueTools 全部静默无效——开关"看起来开了"，
// 工具一个都没注册，运行期没有任何提示。现在必须在注入 logger 里给出警告。
func TestFix_BuiltinToolsWarnWithoutProvider(t *testing.T) {
	// 模拟宿主未 import builtin：清掉全局 provider，测完恢复。
	savedBuiltin := builtinToolsProvider
	savedTask := taskToolsProvider
	savedIssue := issueToolsProvider
	savedPlan := planToolsProvider
	savedBg := bgTaskToolsProvider
	savedAsk := askToolsProvider
	builtinToolsProvider = nil
	taskToolsProvider = nil
	issueToolsProvider = nil
	planToolsProvider = nil
	bgTaskToolsProvider = nil
	askToolsProvider = nil
	defer func() {
		builtinToolsProvider = savedBuiltin
		taskToolsProvider = savedTask
		issueToolsProvider = savedIssue
		planToolsProvider = savedPlan
		bgTaskToolsProvider = savedBg
		askToolsProvider = savedAsk
	}()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	New(
		WithLogger(logger),
		WithBuiltinTools(),
		WithTaskTools(),
		WithIssueTools(),
		WithPlanTools(),
		WithBgTaskTools(),
		WithAskTools(),
	)

	out := buf.String()
	for _, opt := range []string{"WithBuiltinTools", "WithTaskTools", "WithIssueTools", "WithPlanTools", "WithBgTaskTools", "WithAskTools"} {
		if !strings.Contains(out, opt) {
			t.Errorf("%s 未注册时没有警告日志, output=%q", opt, out)
		}
	}
	if !strings.Contains(out, "GoAgent/builtin") {
		t.Errorf("警告未包含修复指引（空白导入路径）, output=%q", out)
	}
}

// provider 正常注册时不应产生误报警告。
func TestFix_BuiltinToolsNoWarnWithProvider(t *testing.T) {
	// 不依赖测试二进制是否 import 了 builtin：直接注入假 provider。
	savedBuiltin := builtinToolsProvider
	savedAsk := askToolsProvider
	builtinToolsProvider = func() []NamedTool { return nil }
	askToolsProvider = func() []NamedTool { return nil }
	defer func() {
		builtinToolsProvider = savedBuiltin
		askToolsProvider = savedAsk
	}()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	New(WithLogger(logger), WithBuiltinTools(), WithAskTools())

	if out := buf.String(); strings.Contains(out, "未检测到 builtin") {
		t.Errorf("provider 已注册却出现误报警告, output=%q", out)
	}
}
