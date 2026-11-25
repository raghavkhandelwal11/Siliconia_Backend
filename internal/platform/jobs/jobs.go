// Package jobs wires the asynq queues, the outbox publisher and the periodic
// scheduler used by the worker (LLD 11).
package jobs

import (
	"context"
	"errors"
	"fmt"

	"github.com/hibiken/asynq"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/outbox"
)

// Queue names and their priority weights.
const (
	QueueCritical      = "critical"
	QueueDefault       = "default"
	QueueNotifications = "notifications"
	QueueLow           = "low"
)

// Queues gives critical work (payments, webhooks, launches) the most capacity.
var Queues = map[string]int{QueueCritical: 6, QueueDefault: 3, QueueNotifications: 2, QueueLow: 1}

// EventTaskType is the asynq task type for an outbox topic, e.g. "event:order.confirmed".
func EventTaskType(topic string) string { return "event:" + topic }

// RedisOpt converts a redis:// URL for asynq.
func RedisOpt(url string) (asynq.RedisConnOpt, error) {
	opt, err := asynq.ParseRedisURI(url)
	if err != nil {
		return nil, fmt.Errorf("jobs: parse redis url: %w", err)
	}
	return opt, nil
}

// OutboxPublisher enqueues each event once. The event ID is the asynq task ID,
// so a repeated publish after a crash is ignored instead of duplicated.
func OutboxPublisher(client *asynq.Client) outbox.Publisher {
	return func(ctx context.Context, e outbox.Event) error {
		task := asynq.NewTask(EventTaskType(e.Topic), e.Payload)
		_, err := client.EnqueueContext(ctx, task,
			asynq.TaskID(e.ID.String()),
			asynq.Queue(QueueDefault),
			asynq.MaxRetry(10),
		)
		if errors.Is(err, asynq.ErrTaskIDConflict) {
			return nil
		}
		return err
	}
}

// Periodic is one scheduled job. Spec uses cron syntax in IST.
type Periodic struct {
	Spec  string
	Type  string
	Queue string
}

// Schedule lists the periodic jobs. Modules add theirs as they are built
// (reservation sweeper, Electrons maturation, payouts and so on).
var Schedule = []Periodic{
	{Spec: "@every 1m", Type: TypeHeartbeat, Queue: QueueLow},
	{Spec: "@every 30s", Type: TypeExpireOrders, Queue: QueueCritical},
	{Spec: "15 3 * * *", Type: TypeCloseIssues, Queue: QueueLow},
	{Spec: "5 * * * *", Type: TypeMatureElectrons, Queue: QueueDefault},
	{Spec: "0 1 * * *", Type: TypeExpireElectrons, Queue: QueueDefault},
	{Spec: "0 9 * * *", Type: TypeRenewMemberships, Queue: QueueCritical},
	{Spec: "@every 5s", Type: TypeLaunchTick, Queue: QueueCritical},
	{Spec: "@every 1s", Type: TypeLaunchAdmit, Queue: QueueCritical},
	{Spec: "0 0 * * 1", Type: TypeProductsOfWeek, Queue: QueueLow},
	{Spec: "0 2 1 * *", Type: TypeSettlePools, Queue: QueueDefault},
	{Spec: "0 6 * * 1", Type: TypeCashoutBatch, Queue: QueueDefault},
	{Spec: "30 3 * * *", Type: TypeDemoOverdue, Queue: QueueLow},
}

// TypeHeartbeat proves the scheduler and worker are alive.
const TypeHeartbeat = "system:heartbeat"

// TypeExpireOrders expires unpaid orders and releases their stock (LLD 8.3).
const TypeExpireOrders = "orders:expire"

// M3 jobs (LLD 11).
const (
	TypeMatureElectrons  = "electrons:mature"
	TypeExpireElectrons  = "electrons:expire"
	TypeRenewMemberships = "membership:renew"
	TypeLaunchTick       = "launches:tick"
	TypeLaunchAdmit      = "launches:admit"
)

// M4 jobs (LLD 11).
const (
	TypeProductsOfWeek = "discovery:products-of-week"
	TypeSettlePools    = "creators:settle-pools"
	TypeCashoutBatch   = "creators:cashout-batch"
	TypeDemoOverdue    = "creators:demo-overdue"
)

// TypeCloseIssues closes issues resolved more than 7 days ago (LLD 10).
const TypeCloseIssues = "issues:close-resolved"
