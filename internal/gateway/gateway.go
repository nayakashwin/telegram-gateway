package gateway

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/nayakashwin/telegram-gateway/internal/config"
	"github.com/nayakashwin/telegram-gateway/internal/metrics"
	"github.com/nayakashwin/telegram-gateway/internal/store"
	"github.com/nayakashwin/telegram-gateway/internal/telegram"
)

// Gateway coordinates Telegram polling and outbox delivery.
type Gateway struct {
	cfg     *config.Config
	store   *store.Store
	client  *telegram.Client
	logger  *slog.Logger
	metrics *metrics.Metrics
}

// New creates a Gateway. m may be nil.
func New(cfg *config.Config, st *store.Store, client *telegram.Client, logger *slog.Logger, m *metrics.Metrics) *Gateway {
	return &Gateway{cfg: cfg, store: st, client: client, logger: logger, metrics: m}
}

// Run starts the poll loop and outbox worker, blocking until ctx is done.
func (g *Gateway) Run(ctx context.Context) error {
	var wg sync.WaitGroup

	wg.Add(2)
	go func() { defer wg.Done(); g.pollLoop(ctx) }()
	go func() { defer wg.Done(); g.outboxWorker(ctx) }()

	<-ctx.Done()
	wg.Wait()
	return nil
}

// pollLoop long-polls Telegram and stores incoming messages.
func (g *Gateway) pollLoop(ctx context.Context) {
	var offset int64

	// Verify the token once at startup.
	if _, err := g.client.GetMe(ctx); err != nil {
		g.logger.Error("getMe failed (check TELEGRAM_BOT_TOKEN)", "error", err)
	}

	for {
		if ctx.Err() != nil {
			return
		}

		updates, err := g.client.GetUpdates(ctx, offset, g.cfg.PollInterval)
		if err != nil {
			g.logger.Error("getUpdates failed", "error", err)
			select {
			case <-time.After(3 * time.Second):
			case <-ctx.Done():
				return
			}
			continue
		}

		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			g.handleIncoming(ctx, u)
		}
	}
}

// handleIncoming stores a received Telegram message if the chat is whitelisted.
func (g *Gateway) handleIncoming(ctx context.Context, u telegram.Update) {
	m := u.Message
	if m == nil {
		return
	}

	if !g.cfg.IsAllowedChat(m.Chat.ID) {
		g.logger.Info("ignoring message from non-whitelisted chat",
			"chat_id", m.Chat.ID, "from", m.From.ID)
		return
	}

	name := m.From.FirstName
	if m.From.LastName != "" {
		name += " " + m.From.LastName
	}

	_, err := g.store.InsertIncoming(ctx, store.Message{
		ChatID:    m.Chat.ID,
		MessageID: m.MessageID,
		FromID:    m.From.ID,
		FromName:  name,
		Text:      m.Text,
		Status:    "received",
	})
	if err != nil {
		g.logger.Error("store incoming message", "error", err)
		return
	}

	g.logger.Info("received message",
		"chat_id", m.Chat.ID, "from", m.From.ID, "text", m.Text)
}

// outboxWorker periodically claims and sends pending outbox rows.
func (g *Gateway) outboxWorker(ctx context.Context) {
	// Seed the backlog gauge once at startup.
	g.refreshBacklog(ctx)

	ticker := time.NewTicker(time.Duration(g.cfg.RetryInterval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.processOutbox(ctx)
			g.refreshBacklog(ctx)
		}
	}
}

func (g *Gateway) refreshBacklog(ctx context.Context) {
	if g.metrics == nil {
		return
	}
	counts, err := g.store.OutboxStatusCounts(ctx)
	if err != nil {
		g.logger.Error("outbox status counts", "error", err)
		return
	}
	g.metrics.SetOutboxBacklog(counts)
}

// processOutbox claims one outbox row and attempts to send it.
func (g *Gateway) processOutbox(ctx context.Context) {
	if err := g.store.ResetExpiredLocks(ctx, time.Now()); err != nil {
		g.logger.Error("reset expired locks", "error", err)
	}

	item, err := g.store.ClaimNextOutbox(ctx, time.Now(), g.cfg.RetryBackoff)
	if err != nil {
		g.logger.Error("claim outbox", "error", err)
		return
	}
	if item == nil {
		return
	}

	if g.metrics != nil {
		g.metrics.ObserveOutboxAttempt()
	}
	var replyTo int64
	if item.ReplyToMessageID != nil {
		replyTo = *item.ReplyToMessageID
	}
	_, err = g.client.SendMessage(ctx, item.ChatID, item.Text, replyTo)
	if err == nil {
		g.logger.Info("sent message", "outbox_id", item.ID, "chat_id", item.ChatID)
		// Use a context that survives parent cancellation so the row is never
		// left in 'processing' after shutdown.
		markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := g.store.MarkOutboxSent(markCtx, item.ID); err != nil {
			g.logger.Error("mark outbox sent", "error", err)
		}
		if g.metrics != nil {
			g.metrics.ObserveOutboxStatus("sent")
		}
		return
	}

	g.logger.Error("send failed", "outbox_id", item.ID, "error", err)
	attempts := item.Attempts + 1
	backoff := g.cfg.RetryBackoff * int64(attempts)
	markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := g.store.MarkOutboxFailed(markCtx, item.ID, attempts, g.cfg.MaxRetries, backoff, err.Error()); err != nil {
		g.logger.Error("mark outbox failed", "error", err)
	}
	if g.metrics != nil {
		status := "failed"
		if attempts >= g.cfg.MaxRetries {
			status = "dead"
		}
		g.metrics.ObserveOutboxStatus(status)
	}
}
