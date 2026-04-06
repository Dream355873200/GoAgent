package permission

// SafeYoloTools 是不需要 YOLO 分类的工具名集合。
// 对齐 Claude Code SAFE_YOLO_ALLOWLISTED_TOOLS。
// 这些工具跳过 YOLO 分类直接允许。
var SafeYoloTools = map[string]bool{
	// 只读文件操作
	"Read": true,
	"Grep": true,
	"Glob": true,

	// Task/Plan 管理
	"TaskCreate": true,
	"TaskGet":    true,
	"TaskList":   true,
	"TaskUpdate": true,

	// UI 工具
	"AskUserQuestion": true,
	"EnterPlanMode":   true,
	"ExitPlanMode":    true,
}

// IsSafeYoloTool 检查工具是否在安全白名单中。
func IsSafeYoloTool(name string) bool {
	return SafeYoloTools[name]
}
