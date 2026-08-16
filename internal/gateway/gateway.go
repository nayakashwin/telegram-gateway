package gateway

import (
	"context"
	"log/slog"
	"strings"
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
	clients map[string]*telegram.Client // keyed by lowercased bot name
	logger  *slog.Logger
	metrics *metrics.Metrics
}

// New creates a Gateway. client is the default bot's client. m may be nil.
// Additional bots must be wired with RegisterBot before Run.
func New(cfg *config.Config, st *store.Store, client *telegram.Client, logger *slog.Logger, m *metrics.Metrics) *Gateway {
	g := &Gateway{
		cfg:     cfg,
		store:   st,
		clients: map[string]*telegram.Client{},
		logger:  logger,
		metrics: m,
	}
	if b := cfg.DefaultBot(); b != nil && client != nil {
		g.clients[b.Name] = client
	}
	return g
}

// RegisterBot attaches a client for a named bot. The name must match one in
// cfg.Bots; main wires every configured bot this way.
func (g *Gateway) RegisterBot(name string, client *telegram.Client) {
	g.clients[strings.ToLower(name)] = client
}

func (g *Gateway) clientFor(name string) *telegram.Client {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		if b := g.cfg.DefaultBot(); b != nil {
			name = b.Name
		}
	}
	return g.clients[name]
}

// Run starts a poll loop per bot and the outbox worker, blocking until ctx is done.
func (g *Gateway) Run(ctx context.Context) error {
	var wg sync.WaitGroup

	for _, b := range g.cfg.Bots {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			g.pollLoop(ctx, name)
		}(b.Name)
	}

	wg.Add(1)
	go func() { defer wg.Done(); g.outboxWorker(ctx) }()

	<-ctx.Done()
	wg.Wait()
	return nil
}

// pollLoop long-polls Telegram for updates for a single bot and stores incoming
// messages. Each bot keeps its own update offset.
func (g *Gateway) pollLoop(ctx context.Context, botName string) {
	client := g.clientFor(botName)
	if client == nil {
		g.logger.Error("poll: no client for bot", "bot", botName)
		return
	}

	var offset int64

	// Verify the token once at startup.
	if _, err := client.GetMe(ctx); err != nil {
		g.logger.Error("getMe failed (check TELEGRAM_BOT_TOKEN)", "bot", botName, "error", err)
	}

	for {
		if ctx.Err() != nil {
			return
		}

		updates, err := client.GetUpdates(ctx, offset, g.cfg.PollInterval)
		if err != nil {
			g.logger.Error("getUpdates failed", "bot", botName, "error", err)
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
			g.handleIncoming(ctx, u, botName)
		}
	}
}

// handleIncoming stores a received Telegram message if the chat is whitelisted.
func (g *Gateway) handleIncoming(ctx context.Context, u telegram.Update, botName string) {
	m := u.Message
	if m == nil {
		return
	}

	if !g.cfg.IsAllowedChat(m.Chat.ID) {
		g.logger.Info("ignoring message from non-whitelisted chat",
			"bot", botName, "chat_id", m.Chat.ID, "from", m.From.ID)
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
		g.logger.Error("store incoming message", "bot", botName, "error", err)
		return
	}

	g.logger.Info("received message",
		"bot", botName, "chat_id", m.Chat.ID, "from", m.From.ID, "text", m.Text)
}

// outboxWorker sends pending outbox rows. It wakes immediately whenever a new
// row is enqueued (via LISTEN/NOTIFY), draining the whole queue per wake, and
// falls back to a periodic sweep for retries and rows inserted while the
// listener was down.
func (g *Gateway) outboxWorker(ctx context.Context) {
	// Seed the backlog gauge once at startup.
	g.refreshBacklog(ctx)

	listener, err := g.store.NewOutboxListener(ctx)
	if err != nil {
		g.logger.Error("outbox listen", "error", err)
	}
	if listener != nil {
		defer func() { listener.Close() }()
	}

	sweep := time.NewTicker(time.Duration(g.cfg.RetryInterval) * time.Second)
	defer sweep.Stop()

	// Notification wait deadline; also bounds idle wake-ups so retries and any
	// notifications missed between a wake and processing are still picked up.
	const waitDeadline = 2 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if listener != nil {
			notified, err := listener.Wait(ctx, waitDeadline)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				g.logger.Error("outbox notify wait", "error", err)
				if l, lerr := g.store.NewOutboxListener(ctx); lerr != nil {
					g.logger.Error("outbox relisten", "error", lerr)
				} else {
					listener.Close()
					listener = l
				}
				continue
			}
			if notified {
				g.processOutbox(ctx)
				g.refreshBacklog(ctx)
			}
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
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

// processOutbox claims and sends all currently pending outbox rows. It stops
// draining on the first failed send so the row's retry lock is honored; the
// next wake or sweep retries it and continues with the remainder.
func (g *Gateway) processOutbox(ctx context.Context) {
	if err := g.store.ResetExpiredLocks(ctx, time.Now()); err != nil {
		g.logger.Error("reset expired locks", "error", err)
	}

	for {
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
		client := g.clientFor(item.Bot)
		if client == nil {
			g.logger.Error("no client for outbox bot; marking dead", "outbox_id", item.ID, "bot", item.Bot)
			markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			if err := g.store.MarkOutboxFailed(markCtx, item.ID, g.cfg.MaxRetries, g.cfg.MaxRetries, 0, "unknown bot: "+item.Bot); err != nil {
				g.logger.Error("mark outbox failed", "error", err)
			}
			cancel()
			if g.metrics != nil {
				g.metrics.ObserveOutboxStatus("dead")
			}
			continue
		}
		_, err = client.SendMessage(ctx, item.ChatID, item.Text, replyTo)
		if err != nil {
			g.logger.Error("send failed", "outbox_id", item.ID, "error", err)
			attempts := item.Attempts + 1
			backoff := g.cfg.RetryBackoff * int64(attempts)
			markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			if err := g.store.MarkOutboxFailed(markCtx, item.ID, attempts, g.cfg.MaxRetries, backoff, err.Error()); err != nil {
				g.logger.Error("mark outbox failed", "error", err)
			}
			cancel()
			if g.metrics != nil {
				status := "failed"
				if attempts >= g.cfg.MaxRetries {
					status = "dead"
				}
				g.metrics.ObserveOutboxStatus(status)
			}
			return
		}

		g.logger.Info("sent message", "outbox_id", item.ID, "chat_id", item.ChatID)
		// Use a context that survives parent cancellation so the row is never
		// left in 'processing' after shutdown.
		markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		if err := g.store.MarkOutboxSent(markCtx, item.ID); err != nil {
			g.logger.Error("mark outbox sent", "error", err)
		}
		cancel()
		if g.metrics != nil {
			g.metrics.ObserveOutboxStatus("sent")
		}
	}
}
