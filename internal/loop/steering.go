// Package loop — 插话源接口。
//
// loop 不感知插话通道的实现（内存队列/跨进程桥接皆可），只认该接口：
// 每个工具批结束边界调用 DrainGuides 取走待注入消息，追加为 user
// 消息进入下一次模型请求。宿主实现此接口并经 Config.Steering 注入。
package loop

import "context"

// SteerSource 会话插话源（guide 车道）。
//
// DrainGuides 取走指定会话的全部待注入插话消息（取走即清空）。
// 返回 nil 表示没有待注入消息。实现必须并发安全——loop 的 run
// goroutine 是唯一调用方，但同一 hub 可能服务多个会话。
type SteerSource interface {
	DrainGuides(sessionID string) []string
}

type childSteerKey struct{}

// WithChildSteering 把子代理运行自己的插话车道放进 ctx：紧接着派生的那个
// 子循环（agent.Runner 经 TakeChildSteering 取走）在自己的工具批边界消费它。
// 子循环不继承父运行的插话来源。
func WithChildSteering(ctx context.Context, src SteerSource) context.Context {
	return context.WithValue(ctx, childSteerKey{}, src)
}

// TakeChildSteering 取出 WithChildSteering 放入的车道，并返回清除了它的 ctx：
// 子循环里的工具再派生的循环不会误用同一条车道。
func TakeChildSteering(ctx context.Context) (SteerSource, context.Context) {
	src, _ := ctx.Value(childSteerKey{}).(SteerSource)
	if src == nil {
		return nil, ctx
	}
	return src, context.WithValue(ctx, childSteerKey{}, nil)
}
