// Package cron 实现 session 内的 cron 调度系统。
//
// 对齐 Claude Code 的 CronCreate/CronDelete/CronList 工具。
// 支持标准 5-field cron 表达式、jitter、session-only 生命周期、3 天自动过期。
//
// 所有 job 仅存在于当前 session 内存中，session 结束即清除。
package cron

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Job 表示一个 cron 任务。
type Job struct {
	// ID 是任务的唯一标识符。
	ID string `json:"id"`

	// Cron 是标准 5-field cron 表达式: minute hour dom month dow。
	Cron string `json:"cron"`

	// Prompt 是触发时执行的 prompt。
	Prompt string `json:"prompt"`

	// Recurring 是否为循环任务。false 表示一次性任务。
	Recurring bool `json:"recurring"`

	// CreatedAt 是任务创建时间。
	CreatedAt time.Time `json:"created_at"`

	// ExpiresAt 是任务过期时间（循环任务 3 天后过期）。
	ExpiresAt time.Time `json:"expires_at"`

	// LastFired 是上次触发时间。
	LastFired *time.Time `json:"last_fired,omitempty"`

	// Fired 是否已触发（一次性任务用）。
	Fired bool `json:"fired"`
}

// IsExpired 返回任务是否已过期。
func (j *Job) IsExpired() bool {
	return time.Now().After(j.ExpiresAt)
}

// Scheduler 管理 cron 任务。
type Scheduler struct {
	mu   sync.RWMutex
	jobs map[string]*Job

	nextID int
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// onFire 是任务触发时的回调。
	onFire func(job *Job)
}

// NewScheduler 创建一个新的 cron 调度器。
func NewScheduler(onFire func(job *Job)) *Scheduler {
	return &Scheduler{
		jobs:   make(map[string]*Job),
		onFire: onFire,
	}
}

// Create 创建一个新的 cron 任务。
func (s *Scheduler) Create(cronExpr, prompt string, recurring bool) (*Job, error) {
	// 验证 cron 表达式。
	if _, err := parseCron(cronExpr); err != nil {
		return nil, fmt.Errorf("无效的 cron 表达式: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextID++
	id := fmt.Sprintf("cron_%d", s.nextID)

	now := time.Now()
	expiresAt := now.Add(72 * time.Hour) // 3 天过期
	if !recurring {
		expiresAt = now.Add(24 * time.Hour) // 一次性任务 24 小时过期
	}

	job := &Job{
		ID:        id,
		Cron:      cronExpr,
		Prompt:    prompt,
		Recurring: recurring,
		CreatedAt: now,
		ExpiresAt: expiresAt,
	}

	s.jobs[id] = job
	return job, nil
}

// Delete 删除指定 ID 的任务。
func (s *Scheduler) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.jobs[id]; !ok {
		return fmt.Errorf("cron 任务 %s 不存在", id)
	}
	delete(s.jobs, id)
	return nil
}

// List 返回所有活跃的任务。
func (s *Scheduler) List() []*Job {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*Job
	for _, j := range s.jobs {
		if !j.IsExpired() && !j.Fired {
			result = append(result, j)
		}
	}
	return result
}

// Start 启动调度器后台 goroutine。
func (s *Scheduler) Start(ctx context.Context) {
	ctx, s.cancel = context.WithCancel(ctx)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.loop(ctx)
	}()
}

// Stop 停止调度器。
func (s *Scheduler) Stop() {
	if s.cancel != nil {
		s.cancel()
		s.wg.Wait()
	}
}

// loop 是调度器主循环，每分钟检查一次。
func (s *Scheduler) loop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.check(now)
		}
	}
}

// check 检查并触发到期的任务。
func (s *Scheduler) check(now time.Time) {
	s.mu.Lock()

	var toFire []*Job
	var toDelete []string

	for id, job := range s.jobs {
		// 清理过期任务。
		if job.IsExpired() {
			toDelete = append(toDelete, id)
			continue
		}

		// 清理已触发的一次性任务。
		if job.Fired {
			toDelete = append(toDelete, id)
			continue
		}

		// 检查是否匹配当前时间。
		if matches(job.Cron, now) {
			// 添加 jitter。
			jitter := calculateJitter(job)
			fireTime := now.Add(jitter)

			// 只在 jitter 后仍匹配时触发。
			if job.LastFired == nil || fireTime.Sub(*job.LastFired) > 50*time.Second {
				toFire = append(toFire, job)
				t := now
				job.LastFired = &t
				if !job.Recurring {
					job.Fired = true
				}
			}
		}
	}

	for _, id := range toDelete {
		delete(s.jobs, id)
	}

	s.mu.Unlock()

	// 在锁外触发回调。
	for _, job := range toFire {
		if s.onFire != nil {
			s.onFire(job)
		}
	}
}

// calculateJitter 计算任务的 jitter。
func calculateJitter(job *Job) time.Duration {
	if !job.Recurring {
		// 一次性任务: :00/:30 时 ±90s。
		return time.Duration(rand.Intn(90)) * time.Second
	}
	// 循环任务: ±10% period, 最大 15 分钟。
	maxJitter := 15 * time.Minute
	jitter := time.Duration(rand.Intn(int(maxJitter.Seconds()))) * time.Second
	return jitter
}

// cronField 表示 cron 表达式的一个字段。
type cronField struct {
	values map[int]bool // 匹配的值集合
	any    bool         // 是否为 *（匹配所有）
}

// parseCron 解析 5-field cron 表达式。
func parseCron(expr string) ([]cronField, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return nil, fmt.Errorf("需要 5 个字段，得到 %d", len(parts))
	}

	maxValues := []int{59, 23, 31, 12, 6} // minute, hour, dom, month, dow
	minValues := []int{0, 0, 1, 1, 0}

	fields := make([]cronField, 5)
	for i, part := range parts {
		f, err := parseField(part, minValues[i], maxValues[i])
		if err != nil {
			return nil, fmt.Errorf("字段 %d (%s) 解析失败: %w", i, part, err)
		}
		fields[i] = f
	}

	return fields, nil
}

// parseField 解析 cron 表达式的单个字段。
func parseField(field string, min, max int) (cronField, error) {
	if field == "*" {
		return cronField{any: true}, nil
	}

	values := make(map[int]bool)

	// 处理逗号分隔。
	for _, part := range strings.Split(field, ",") {
		// 处理 */N 步进。
		if strings.HasPrefix(part, "*/") {
			step, err := strconv.Atoi(part[2:])
			if err != nil || step <= 0 {
				return cronField{}, fmt.Errorf("无效步进: %s", part)
			}
			for i := min; i <= max; i += step {
				values[i] = true
			}
			continue
		}

		// 处理 N-M 范围。
		if strings.Contains(part, "-") {
			rangeParts := strings.SplitN(part, "-", 2)
			low, err1 := strconv.Atoi(rangeParts[0])
			high, err2 := strconv.Atoi(rangeParts[1])
			if err1 != nil || err2 != nil || low > high {
				return cronField{}, fmt.Errorf("无效范围: %s", part)
			}
			for i := low; i <= high; i++ {
				values[i] = true
			}
			continue
		}

		// 单个值。
		val, err := strconv.Atoi(part)
		if err != nil {
			return cronField{}, fmt.Errorf("无效值: %s", part)
		}
		if val < min || val > max {
			return cronField{}, fmt.Errorf("值 %d 超出范围 [%d, %d]", val, min, max)
		}
		values[val] = true
	}

	return cronField{values: values}, nil
}

// matches 检查 cron 表达式是否匹配给定时间。
func matches(cronExpr string, t time.Time) bool {
	fields, err := parseCron(cronExpr)
	if err != nil {
		return false
	}

	checks := []int{
		t.Minute(),
		t.Hour(),
		t.Day(),
		int(t.Month()),
		int(t.Weekday()),
	}

	for i, field := range fields {
		if field.any {
			continue
		}
		if !field.values[checks[i]] {
			return false
		}
	}

	return true
}

// NextFire 计算下次触发时间。
func NextFire(cronExpr string, from time.Time) (time.Time, error) {
	_, err := parseCron(cronExpr)
	if err != nil {
		return time.Time{}, err
	}

	// 从下一分钟开始搜索。
	t := from.Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < 525960; i++ { // 最多搜索 1 年
		if matches(cronExpr, t) {
			return t, nil
		}
		t = t.Add(time.Minute)
	}

	return time.Time{}, fmt.Errorf("1 年内没有找到匹配时间")
}
