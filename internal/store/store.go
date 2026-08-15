package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrNoRows is returned when a query matched no rows.
	ErrNoRows = errors.New("no rows")
	// ErrOutboxLocked indicates a concurrent worker already holds the row.
	ErrOutboxLocked = errors.New("outbox row locked")
)

// Message is a single Telegram message stored by the gateway.
type Message struct {
	ID         int64     `json:"id"`
	MessageID  int64     `json:"message_id"` // Telegram message id, usable as reply_to_message_id
	ChatID     int64     `json:"chat_id"`
	FromID     int64     `json:"from_id"`
	FromName   string    `json:"from_name"`
	Text       string    `json:"text"`
	Status     string    `json:"status"`
	ReceivedAt time.Time `json:"received_at"`
	CreatedAt  time.Time `json:"created_at"`
}

// OutboxItem is a row in the outbox queue waiting to be sent to Telegram.
type OutboxItem struct {
	ID               int64      `json:"id"`
	ChatID           int64      `json:"chat_id"`
	Text             string     `json:"text"`
	Status           string     `json:"status"`
	ErrorMessage     string     `json:"error_message,omitempty"`
	Attempts         int        `json:"attempts"`
	Source           string     `json:"source"`
	ReplyToMessageID *int64     `json:"reply_to_message_id,omitempty"`
	SentAt           *time.Time `json:"sent_at"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// PoolConfig tunes the pgx connection pool.
type PoolConfig struct {
	MinConns        int32
	MaxConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
}

// DefaultPoolConfig returns sensible production pool defaults.
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		MinConns:        1,
		MaxConns:        10,
		MaxConnLifetime: 30 * time.Minute,
		MaxConnIdleTime: 5 * time.Minute,
	}
}

// Store wraps a pgx pool with the gateway's data access.
type Store struct {
	pool *pgxpool.Pool
}

// New connects to Postgres and returns a ready Store.
func New(ctx context.Context, databaseURL string, pc PoolConfig) (*Store, error) {
	if pc.MaxConns <= 0 || pc.MinConns < 0 || pc.MinConns > pc.MaxConns {
		return nil, fmt.Errorf("invalid pool config: min=%d max=%d", pc.MinConns, pc.MaxConns)
	}
	// pgx v5.10 treats a zero MaxConnIdleTime as "reap immediately"; require a
	// positive idle time so pools with MinConns>0 don't churn connections.
	if pc.MaxConnIdleTime <= 0 {
		pc.MaxConnIdleTime = DefaultPoolConfig().MaxConnIdleTime
	}
	if pc.MaxConnLifetime <= 0 {
		pc.MaxConnLifetime = DefaultPoolConfig().MaxConnLifetime
	}

	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse pool config: %w", err)
	}
	cfg.MinConns = pc.MinConns
	cfg.MaxConns = pc.MaxConns
	cfg.MaxConnLifetime = pc.MaxConnLifetime
	cfg.MaxConnIdleTime = pc.MaxConnIdleTime

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	s := &Store{pool: pool}
	if err := s.Migrate(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Pool exposes the underlying pool for metrics collection.
func (s *Store) Pool() *pgxpool.Pool {
	return s.pool
}

// Stat returns the current pool statistics (implements metrics.PoolStatter).
func (s *Store) Stat() *pgxpool.Stat {
	return s.pool.Stat()
}

// Migrate creates the schema if it does not yet exist.
// It serializes on a Postgres advisory lock so concurrent starters
// (e.g. multiple gateway instances) do not race DDL.
func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire for migrate: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(747401)`); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(747401)`) //nolint:errcheck

	if _, err := conn.Exec(ctx, schema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

const schema = `
CREATE TABLE IF NOT EXISTS incoming_messages (
    id          BIGSERIAL PRIMARY KEY,
    chat_id     BIGINT NOT NULL,
    from_id     BIGINT NOT NULL,
    from_name   TEXT NOT NULL DEFAULT '',
    text        TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'received',
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_incoming_created ON incoming_messages (created_at);
CREATE INDEX IF NOT EXISTS idx_incoming_chat_created ON incoming_messages (chat_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_incoming_from_created ON incoming_messages (from_id, created_at DESC);
ALTER TABLE incoming_messages ADD COLUMN IF NOT EXISTS received_at TIMESTAMPTZ NOT NULL DEFAULT now();
-- Telegram message id of the incoming message; used by consumers to reply.
ALTER TABLE incoming_messages ADD COLUMN IF NOT EXISTS tg_message_id BIGINT NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS outbox (
    id            BIGSERIAL PRIMARY KEY,
    chat_id       BIGINT NOT NULL,
    text          TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'pending',
    attempts      INT NOT NULL DEFAULT 0,
    error_message TEXT NOT NULL DEFAULT '',
    sent_at       TIMESTAMPTZ,
    locked_until  TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_outbox_status ON outbox (status, id);
ALTER TABLE outbox ADD COLUMN IF NOT EXISTS sent_at TIMESTAMPTZ;

-- Source attribution: where an outbound message originated. 'api' = REST API,
-- 'internal' = direct DB insert by the application. Helps detect forged sends.
ALTER TABLE outbox ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'unknown';

-- Optional Telegram message id this outbound message replies to.
ALTER TABLE outbox ADD COLUMN IF NOT EXISTS reply_to_message_id BIGINT;

CREATE OR REPLACE FUNCTION notify_outbox() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('outbox_channel', row_to_json(NEW)::text);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS outbox_notify ON outbox;
CREATE TRIGGER outbox_notify
AFTER INSERT OR UPDATE OF status ON outbox
FOR EACH ROW EXECUTE FUNCTION notify_outbox();
`

// Ping verifies the database connection.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// Close releases the connection pool.
func (s *Store) Close() {
	s.pool.Close()
}

// InsertIncoming stores a received message and returns its id.
func (s *Store) InsertIncoming(ctx context.Context, m Message) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO incoming_messages (chat_id, from_id, from_name, text, status, tg_message_id, received_at)
		 VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7, now())) RETURNING id`,
		m.ChatID, m.FromID, m.FromName, m.Text, m.Status, m.MessageID, nullIfZero(m.ReceivedAt),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert incoming: %w", err)
	}
	return id, nil
}

// nullIfZero returns nil for the zero time so COALESCE can default to now().
func nullIfZero(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// InsertOutbox queues a message for sending. source records where the
// outbound message originated (e.g. "api", "internal") for attribution.
// replyToMessageID, when non-nil, makes the outbound message a Telegram reply
// to that incoming message id.
func (s *Store) InsertOutbox(ctx context.Context, chatID int64, text, source string, replyToMessageID *int64) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO outbox (chat_id, text, status, source, reply_to_message_id) VALUES ($1, $2, 'pending', $3, $4) RETURNING id`,
		chatID, text, source, replyToMessageID,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert outbox: %w", err)
	}
	return id, nil
}

// ClaimNextOutbox atomically claims one sendable outbox row for delivery:
// pending rows, plus failed rows whose retry backoff has elapsed.
func (s *Store) ClaimNextOutbox(ctx context.Context, now time.Time, retryBackoff int64) (*OutboxItem, error) {
	var item OutboxItem
	err := s.pool.QueryRow(ctx, `
		UPDATE outbox
		SET status = 'processing', locked_until = $1 + ($2 * interval '1 second')
		WHERE id = (
			SELECT id FROM outbox
			WHERE (
				status = 'pending'
				OR (status = 'failed' AND locked_until <= $1)
			)
			ORDER BY id
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, chat_id, text, status, attempts, error_message, source, reply_to_message_id, sent_at, created_at`,
		now, retryBackoff,
	).Scan(&item.ID, &item.ChatID, &item.Text, &item.Status, &item.Attempts, &item.ErrorMessage, &item.Source, &item.ReplyToMessageID, &item.SentAt, &item.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("claim outbox: %w", err)
	}
	return &item, nil
}

// MarkOutboxSent marks an outbox row as sent and records the delivery time.
func (s *Store) MarkOutboxSent(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE outbox SET status = 'sent', error_message = '', sent_at = now(), updated_at = now() WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("mark outbox sent: %w", err)
	}
	return nil
}

// MarkOutboxFailed marks an outbox row as failed (retryable after backoff) or dead.
func (s *Store) MarkOutboxFailed(ctx context.Context, id int64, attempts, maxRetries int, backoffSecs int64, errMsg string) error {
	status := "failed"
	if attempts >= maxRetries {
		status = "dead"
	}
	// Dead rows are never retried; failed rows are locked until the backoff
	// elapses and then become claimable again.
	_, err := s.pool.Exec(ctx, `
		UPDATE outbox SET
			status = $2,
			attempts = $3,
			error_message = $4,
			locked_until = CASE
				WHEN $2 = 'dead' THEN now()
				ELSE now() + ($5::int * interval '1 second')
			END,
			updated_at = now()
		WHERE id = $1`,
		id, status, attempts, errMsg, backoffSecs)
	if err != nil {
		return fmt.Errorf("mark outbox failed: %w", err)
	}
	return nil
}

// GetOutboxStatus returns the current status of an outbox row.
func (s *Store) GetOutboxStatus(ctx context.Context, id int64) (string, error) {
	var status string
	err := s.pool.QueryRow(ctx, `SELECT status FROM outbox WHERE id = $1`, id).Scan(&status)
	if err != nil {
		return "", fmt.Errorf("get outbox status: %w", err)
	}
	return status, nil
}

// ResetExpiredLocks returns any 'processing' rows whose lock expired back to 'pending'.
func (s *Store) ResetExpiredLocks(ctx context.Context, now time.Time) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE outbox SET status = 'pending', updated_at = now()
		 WHERE status = 'processing' AND locked_until <= $1`, now)
	if err != nil {
		return fmt.Errorf("reset expired locks: %w", err)
	}
	return nil
}

// ListIncoming returns recent incoming messages, newest first.
func (s *Store) ListIncoming(ctx context.Context, limit int) ([]Message, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, chat_id, from_id, from_name, text, status, tg_message_id, received_at, created_at
		FROM incoming_messages ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list incoming: %w", err)
	}
	defer rows.Close()

	msgs := []Message{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ChatID, &m.FromID, &m.FromName, &m.Text, &m.Status, &m.MessageID, &m.ReceivedAt, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan incoming: %w", err)
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate incoming: %w", err)
	}
	return msgs, nil
}

// IncomingFilter filters incoming messages. Zero-value fields are ignored.
type IncomingFilter struct {
	ChatID   *int64
	FromID   *int64
	FromName string     // ILIKE substring; empty = no filter
	After    *time.Time // created_at > after
	Before   *time.Time // created_at < before
	Limit    int        // 0 = 50; clamped to [1, 500]
}

// QueryIncoming returns incoming messages matching the filter, newest first.
func (s *Store) QueryIncoming(ctx context.Context, f IncomingFilter) ([]Message, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 50
	}

	query := `SELECT id, chat_id, from_id, from_name, text, status, tg_message_id, received_at, created_at
	          FROM incoming_messages WHERE true`
	var args []any
	n := 1
	add := func(clause string, v any) {
		query += fmt.Sprintf(" AND %s = $%d", clause, n)
		args = append(args, v)
		n++
	}
	if f.ChatID != nil {
		add("chat_id", *f.ChatID)
	}
	if f.FromID != nil {
		add("from_id", *f.FromID)
	}
	if f.FromName != "" {
		query += fmt.Sprintf(" AND from_name ILIKE '%%' || $%d || '%%'", n)
		args = append(args, f.FromName)
		n++
	}
	if f.After != nil {
		query += fmt.Sprintf(" AND created_at > $%d", n)
		args = append(args, *f.After)
		n++
	}
	if f.Before != nil {
		query += fmt.Sprintf(" AND created_at < $%d", n)
		args = append(args, *f.Before)
		n++
	}
	query += fmt.Sprintf(" ORDER BY id DESC LIMIT $%d", n)
	args = append(args, f.Limit)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query incoming: %w", err)
	}
	defer rows.Close()

	msgs := []Message{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ChatID, &m.FromID, &m.FromName, &m.Text, &m.Status, &m.MessageID, &m.ReceivedAt, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan incoming: %w", err)
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate incoming: %w", err)
	}
	return msgs, nil
}

// GetOutbox returns a single outbox row. Returns ErrNoRows if not found.
func (s *Store) GetOutbox(ctx context.Context, id int64) (*OutboxItem, error) {
	var item OutboxItem
	err := s.pool.QueryRow(ctx, `
		SELECT id, chat_id, text, status, attempts, error_message, source, reply_to_message_id, sent_at, created_at, updated_at
		FROM outbox WHERE id = $1`, id,
	).Scan(&item.ID, &item.ChatID, &item.Text, &item.Status, &item.Attempts, &item.ErrorMessage, &item.Source, &item.ReplyToMessageID, &item.SentAt, &item.CreatedAt, &item.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoRows
		}
		return nil, fmt.Errorf("get outbox: %w", err)
	}
	return &item, nil
}

// OutboxStatusCounts returns the count of outbox rows grouped by status.
func (s *Store) OutboxStatusCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT status, count(*) FROM outbox GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("outbox status counts: %w", err)
	}
	defer rows.Close()

	counts := map[string]int64{}
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return nil, fmt.Errorf("scan outbox status: %w", err)
		}
		counts[status] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate outbox status: %w", err)
	}
	return counts, nil
}

// Notify delivers an outbox payload to the notify channel.
func (s *Store) Notify(ctx context.Context, payload string) error {
	_, err := s.pool.Exec(ctx, `SELECT pg_notify('outbox_channel', $1)`, payload)
	if err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	return nil
}

// IsUniqueViolation reports whether err is a Postgres unique-constraint error.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
