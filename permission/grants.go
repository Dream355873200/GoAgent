package permission

import (
	"sort"
	"sync"
)

// Grants 是"始终允许"授权的集合。每个权限门默认各有一份；把同一份 Grants
// 交给多个权限门（Gate.SetGrants），授权就在它们之间共享——例如同一会话的
// 多次运行。可并发使用。
type Grants struct {
	mu   sync.RWMutex
	keys map[string]bool
}

// NewGrants 创建一个空的授权集合。
func NewGrants() *Grants {
	return &Grants{keys: make(map[string]bool)}
}

// covers 判断这次调用是否已被某条授权覆盖。
func (s *Grants) covers(toolName, input string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, key := range alwaysAllowCandidates(toolName, input) {
		if s.keys[key] {
			return true
		}
	}
	return false
}

func (s *Grants) add(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[key] = true
}

// Keys 返回全部授权（按字典序）：普通工具为工具名，shell 命令为"工具名 +
// 命令的前一到两个词"。
func (s *Grants) Keys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.keys))
	for k := range s.keys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Clear 撤销全部授权。
func (s *Grants) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = make(map[string]bool)
}
