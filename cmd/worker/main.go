// Command worker relays outbox events, runs queued jobs and the periodic scheduler (LLD 11).
package main

import (
	"context"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/checkout"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/issues"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/notify"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/config"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/jobs"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/logx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/outbox"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/redisx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/messaging"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

func main() {
	log := logx.New("worker")
	cfg, err := config.Load()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("postgres", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	redisOpt, err := jobs.RedisOpt(cfg.RedisURL)
	if err != nil {
		log.Error("redis", "err", err)
		os.Exit(1)
	}

	client := asynq.NewClient(redisOpt)
	defer client.Close()

	// Outbox relay: domain events → queue.
	relay := &outbox.Relay{Pool: pool, Publish: jobs.OutboxPublisher(client), Log: log}
	go relay.Run(ctx)

	// Periodic jobs. The asynq scheduler enqueues each job once per tick.
	scheduler := asynq.NewScheduler(redisOpt, &asynq.SchedulerOpts{Location: mustIST()})
	for _, p := range jobs.Schedule {
		if _, err := scheduler.Register(p.Spec, asynq.NewTask(p.Type, nil), asynq.Queue(p.Queue)); err != nil {
			log.Error("schedule", "type", p.Type, "err", err)
			os.Exit(1)
		}
	}
	go func() {
		if err := scheduler.Run(); err != nil {
			log.Error("scheduler", "err", err)
			stop()
		}
	}()

	// Job handlers. Modules register theirs here as they are built.
	mux := asynq.NewServeMux()
	mux.HandleFunc(jobs.TypeHeartbeat, func(context.Context, *asynq.Task) error {
		log.Debug("heartbeat")
		return nil
	})
	q := store.New(pool)
	checkouts := &checkout.Service{Pool: pool, Q: q, Log: log}
	issueSvc := &issues.Service{Pool: pool, Q: q, Log: log}

	// Notifications: every outbox event ("event:<topic>") goes through the
	// router; topics without a template are acknowledged and dropped.
	rdb, err := redisx.Open(ctx, cfg.RedisURL)
	if err != nil {
		log.Error("redis", "err", err)
		os.Exit(1)
	}
	defer rdb.Close()
	msgs, err := messaging.NewRouter(cfg.ProviderSMS, cfg.ProviderWhatsApp, cfg.ProviderEmail, cfg.SimNotifyURL,
		messaging.BrevoConfig{APIKey: cfg.BrevoAPIKey, From: cfg.EmailFrom, FromName: cfg.EmailFromName}, log)
	if err != nil {
		log.Error("messaging", "err", err)
		os.Exit(1)
	}
	notifier := &notify.Service{Pool: pool, Q: q, Messenger: msgs, PublicURL: cfg.PublicBaseURL, Log: log,
		Publish: func(ctx context.Context, account uuid.UUID, it notify.Item) error {
			return rdb.Publish(ctx, notify.Channel(account), notify.MarshalItem(it)).Err()
		}}
	mux.HandleFunc("event:", func(ctx context.Context, t *asynq.Task) error {
		id, _ := asynq.GetTaskID(ctx)
		return notifier.Handle(ctx, strings.TrimPrefix(t.Type(), "event:"), id, t.Payload())
	})
	mux.HandleFunc(jobs.TypeCloseIssues, func(ctx context.Context, _ *asynq.Task) error {
		n, err := issueSvc.CloseResolved(ctx)
		if n > 0 {
			log.Info("closed resolved issues", "count", n)
		}
		return err
	})
	mux.HandleFunc(jobs.TypeExpireOrders, func(ctx context.Context, _ *asynq.Task) error {
		n, err := checkouts.Sweep(ctx)
		if n > 0 {
			log.Info("expired unpaid orders", "count", n)
		}
		return err
	})

	srv := asynq.NewServer(redisOpt, asynq.Config{Concurrency: 10, Queues: jobs.Queues})
	go func() {
		if err := srv.Run(mux); err != nil {
			log.Error("worker server", "err", err)
			stop()
		}
	}()

	log.Info("worker started", "mode", cfg.Mode)
	<-ctx.Done()
	srv.Shutdown()
	scheduler.Shutdown()
	log.Info("worker stopped")
}
