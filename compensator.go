package goagent

import "context"

// Transaction 应用层事务接口。
// Pipeline worker 执行过程中，所有 DB 写操作（INSERT/UPDATE/DELETE）通过 GORM callback 自动记录。
// Approve 时调用 Commit 确认生效，Reject 时调用 Rollback 逆序撤销所有写操作。
type Transaction interface {
	// Rollback 逆序撤销所有已记录的写操作。
	Rollback(ctx context.Context) error

	// Commit 清空记录，确认所有写操作生效。
	Commit()
}

// TransactionFactory 事务工厂函数。
// Pipeline 每次 worker 执行前调用，创建一个新的 Transaction 实例。
// 业务侧提供实现（如基于 GORM 的应用层事务）。
type TransactionFactory func(ctx context.Context) Transaction

type ctxKeyTransactionType struct{}

var ctxKeyTransaction = ctxKeyTransactionType{}

// WithTransaction 将 Transaction 注入到 context 中。
func WithTransaction(ctx context.Context, tx Transaction) context.Context {
	return context.WithValue(ctx, ctxKeyTransaction, tx)
}

// GetTransaction 从 context 中获取 Transaction。
func GetTransaction(ctx context.Context) Transaction {
	if tx, ok := ctx.Value(ctxKeyTransaction).(Transaction); ok {
		return tx
	}
	return nil
}
