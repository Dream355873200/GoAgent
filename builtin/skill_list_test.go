package builtin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dream355873200/GoAgent"
	"github.com/Dream355873200/GoAgent/skill"
)

// list 动作：合并全局注册表 + 项目目录现场扫描，同名项目版覆盖全局。
func TestSkillListAction(t *testing.T) {
	dir := t.TempDir()
	cmdDir := filepath.Join(dir, ".yume", "commands")
	if err := os.MkdirAll(cmdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cmdDir, "proj-skill.md"), []byte("# s\n\n项目专属技能描述\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := skill.NewRegistry("", "")
	reg.Register(&skill.Skill{Name: "global-skill", Content: "全局技能描述", Source: skill.SourceUser})
	reg.Register(&skill.Skill{Name: "proj-skill", Content: "全局同名（应被项目覆盖）", Source: skill.SourceUser})

	out := listSkills(dir, reg)
	if !strings.Contains(out, "proj-skill: 项目专属技能描述（项目）") {
		t.Errorf("项目 skill 未列出或未被项目版覆盖:\n%s", out)
	}
	if !strings.Contains(out, "global-skill: 全局技能描述（全局）") {
		t.Errorf("全局 skill 未列出:\n%s", out)
	}
	if strings.Contains(out, "全局同名") {
		t.Errorf("同名 skill 应被项目版覆盖:\n%s", out)
	}
}

// 工具描述内嵌全局清单：模型看得见可用技能。
func TestSkillToolDescriptionListsSkills(t *testing.T) {
	reg := skill.NewRegistry("", "")
	reg.Register(&skill.Skill{Name: "testing", Content: "测试策略描述", Source: skill.SourceUser})
	desc := skillToolDescription(reg)
	if !strings.Contains(desc, "testing: 测试策略描述") {
		t.Errorf("描述应含 skill 清单:\n%s", desc)
	}
}

// 项目级 skill 优先于全局（readProjectSkill 现场读取）。
func TestProjectSkillPriority(t *testing.T) {
	dir := t.TempDir()
	cmdDir := filepath.Join(dir, ".yume", "commands")
	if err := os.MkdirAll(cmdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cmdDir, "only-proj.md"), []byte("项目版本内容 $ARGUMENTS"), 0o644); err != nil {
		t.Fatal(err)
	}
	content, ok := readProjectSkill(dir, "only-proj", "参数X")
	if !ok || content != "项目版本内容 参数X" {
		t.Errorf("readProjectSkill 结果错误: %q ok=%v", content, ok)
	}
	// 路径逃逸拒绝
	if _, ok := readProjectSkill(dir, "../escape", ""); ok {
		t.Errorf("路径逃逸应被拒绝")
	}
	_ = goagent.Permission(0) // 保持 goagent 导入引用
}
