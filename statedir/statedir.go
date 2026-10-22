// Package statedir 管理框架在项目（工作）目录下存放运行状态的目录名。
//
// 会话存储（RunCLI / RunHTTP 自动创建时）、计划文件、后台任务输出、项目
// skill（commands）、worktree、会话记忆与动态 pipeline 快照都位于这个目录
// 下。默认 ".yume"；宿主可以用 goagent.WithStateDir 或 Set 改成自己的名字。
package statedir

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// Default 是默认的状态目录名。
const Default = ".yume"

var current atomic.Pointer[string]

// Name 返回当前生效的状态目录名。
func Name() string {
	if p := current.Load(); p != nil {
		return *p
	}
	return Default
}

// Set 设置状态目录名，在整个进程内生效：内置工具、skill 发现等子系统不经过
// App 的配置，统一从这里取。name 必须是单个路径段（不含路径分隔符，不是
// "." 或 ".."）；空字符串恢复默认值。
func Set(name string) error {
	if name == "" {
		current.Store(nil)
		return nil
	}
	if name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") || filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return fmt.Errorf("statedir: %q 不是合法的目录名（必须是单个路径段）", name)
	}
	current.Store(&name)
	return nil
}

// Join 返回 root 下状态目录中的路径，即 filepath.Join(root, Name(), elem...)。
// root 为空时相对于进程当前目录。
func Join(root string, elem ...string) string {
	return filepath.Join(append([]string{root, Name()}, elem...)...)
}
