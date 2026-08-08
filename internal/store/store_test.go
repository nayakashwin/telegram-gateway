package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nayakashwin/telegram-gateway/internal/store"
	"github.com/nayakashwin/telegram-gateway/internal/testdb"
)

// testStore returns a Store in an isolated Postgres schema, or skips if
// TEST_DATABASE_URL is not set.
func testStore(t *testing.T) *store.Store {
	t.Helper()
	return testdb.NewStore(t)
}

func TestIncomingLifecycle(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	id, err := st.InsertIncoming(ctx, store.Message{
		ChatID: 42, FromID: 7, FromName: "Alice", Text: "hello", Status: "received",
	})
	if err != nil {
		t.Fatalf("InsertIncoming: %v", err)
	}
	if id == 0 {
		t.Fatal("InsertIncoming returned id 0")
	}

	msgs, err := st.ListIncoming(ctx, 10)
	if err != nil {
		t.Fatalf("ListIncoming: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	got := msgs[0]
	if got.ChatID != 42 || got.Text != "hello" || got.FromName != "Alice" {
		t.Errorf("message = %+v", got)
	}
	if got.Status != "received" {
		t.Errorf("status = %q", got.Status)
	}
	if got.ReceivedAt.IsZero() {
		t.Error("received_at should be populated")
	}
}

func TestOutboxSendFlow(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	id, err := st.InsertOutbox(ctx, 42, "out", "test")
	if err != nil {
		t.Fatalf("InsertOutbox: %v", err)
	}

	item, err := st.ClaimNextOutbox(ctx, time.Now(), 0)
	if err != nil {
		t.Fatalf("ClaimNextOutbox: %v", err)
	}
	if item == nil || item.ID != id {
		t.Fatalf("claimed = %+v, want id %d", item, id)
	}
	if item.Status != "processing" {
		t.Errorf("status = %q, want processing", item.Status)
	}

	// While processing, nothing else can be claimed.
	again, err := st.ClaimNextOutbox(ctx, time.Now(), 0)
	if err != nil {
		t.Fatalf("second ClaimNextOutbox: %v", err)
	}
	if again != nil {
		t.Errorf("second claim = %+v, want nil", again)
	}

	if err := st.MarkOutboxSent(ctx, id); err != nil {
		t.Fatalf("MarkOutboxSent: %v", err)
	}

	// sent_at should be recorded on delivery.
	sentItem, err := st.GetOutbox(ctx, id)
	if err != nil {
		t.Fatalf("GetOutbox after sent: %v", err)
	}
	if sentItem.SentAt == nil || sentItem.SentAt.IsZero() {
		t.Error("sent_at should be populated after MarkOutboxSent")
	}

	// Sent rows are not claimable.
	sent, err := st.ClaimNextOutbox(ctx, time.Now(), 0)
	if err != nil {
		t.Fatalf("claim after sent: %v", err)
	}
	if sent != nil {
		t.Errorf("claimed sent row = %+v", sent)
	}
}

func TestOutboxRetryBackoff(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	id, _ := st.InsertOutbox(ctx, 42, "retry me", "test")
	item, _ := st.ClaimNextOutbox(ctx, time.Now(), 30)
	if err := st.MarkOutboxFailed(ctx, id, item.Attempts+1, 3, 30, "boom"); err != nil {
		t.Fatalf("MarkOutboxFailed: %v", err)
	}

	// Failed row must NOT be claimable before backoff elapses.
	tooEarly := time.Now().Add(10 * time.Second)
	got, err := st.ClaimNextOutbox(ctx, tooEarly, 30)
	if err != nil {
		t.Fatalf("early claim: %v", err)
	}
	if got != nil {
		t.Fatalf("claimed failed row before backoff: %+v", got)
	}

	// After backoff elapses it becomes claimable again with attempts preserved.
	after := time.Now().Add(35 * time.Second)
	got, err = st.ClaimNextOutbox(ctx, after, 30)
	if err != nil {
		t.Fatalf("late claim: %v", err)
	}
	if got == nil || got.ID != id {
		t.Fatalf("claimed = %+v, want id %d", got, id)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", got.Attempts)
	}
}

func TestOutboxDeadLetter(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	id, _ := st.InsertOutbox(ctx, 42, "will die", "test")
	item, _ := st.ClaimNextOutbox(ctx, time.Now(), 1)
	if err := st.MarkOutboxFailed(ctx, id, item.Attempts+1, 1, 1, "fatal"); err != nil {
		t.Fatalf("MarkOutboxFailed: %v", err)
	}

	got, err := st.ClaimNextOutbox(ctx, time.Now().Add(time.Hour), 1)
	if err != nil {
		t.Fatalf("claim dead: %v", err)
	}
	if got != nil {
		t.Fatalf("claimed dead row: %+v", got)
	}
}

func TestResetExpiredLocks(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	id, _ := st.InsertOutbox(ctx, 42, "stuck", "test")
	// Claim with a zero backoff, simulating a worker that died mid-send.
	if _, err := st.ClaimNextOutbox(ctx, time.Now(), 0); err != nil {
		t.Fatalf("claim: %v", err)
	}

	if err := st.ResetExpiredLocks(ctx, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("ResetExpiredLocks: %v", err)
	}

	item, err := st.ClaimNextOutbox(ctx, time.Now(), 0)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if item == nil || item.ID != id {
		t.Fatalf("reclaimed = %+v, want %d", item, id)
	}
}

func TestNotifyFires(t *testing.T) {
	dsn := testdb.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	st, err := store.New(ctx, dsn, store.DefaultPoolConfig())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	defer pool.Close()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN outbox_channel"); err != nil {
		t.Fatalf("listen: %v", err)
	}

	if _, err := st.InsertOutbox(ctx, 42, "notify me", "test"); err != nil {
		t.Fatalf("InsertOutbox: %v", err)
	}

	notification, err := conn.Conn().WaitForNotification(ctx)
	if err != nil {
		t.Fatalf("WaitForNotification: %v", err)
	}
	if notification.Channel != "outbox_channel" {
		t.Errorf("channel = %q", notification.Channel)
	}
	if notification.Payload == "" {
		t.Error("empty payload")
	}
}

func TestQueryIncomingFilters(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	_, _ = st.InsertIncoming(ctx, store.Message{ChatID: 111, FromID: 111, FromName: "Alice", Text: "a"})
	_, _ = st.InsertIncoming(ctx, store.Message{ChatID: 222, FromID: 222, FromName: "Bob", Text: "b"})
	_, _ = st.InsertIncoming(ctx, store.Message{ChatID: 111, FromID: 111, FromName: "Alice Smith", Text: "c"})

	chat := int64(111)
	msgs, err := st.QueryIncoming(ctx, store.IncomingFilter{ChatID: &chat})
	if err != nil {
		t.Fatalf("QueryIncoming by chat: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("chat filter got %d messages, want 2", len(msgs))
	}

	msgs, err = st.QueryIncoming(ctx, store.IncomingFilter{FromName: "smith"})
	if err != nil {
		t.Fatalf("QueryIncoming by name: %v", err)
	}
	if len(msgs) != 1 || msgs[0].FromName != "Alice Smith" {
		t.Fatalf("name filter got %+v", msgs)
	}

	before := time.Now().Add(time.Minute)
	msgs, err = st.QueryIncoming(ctx, store.IncomingFilter{Before: &before, Limit: 1})
	if err != nil {
		t.Fatalf("QueryIncoming before+limit: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("limit got %d messages, want 1", len(msgs))
	}
}

func TestGetOutbox(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	id, err := st.InsertOutbox(ctx, 42, "track", "test")
	if err != nil {
		t.Fatalf("InsertOutbox: %v", err)
	}
	item, err := st.GetOutbox(ctx, id)
	if err != nil {
		t.Fatalf("GetOutbox: %v", err)
	}
	if item.Status != "pending" || item.ChatID != 42 {
		t.Errorf("item = %+v", item)
	}
	if item.Source != "test" {
		t.Errorf("source = %q, want test", item.Source)
	}

	if _, err := st.GetOutbox(ctx, 999999); err != store.ErrNoRows {
		t.Errorf("missing row err = %v, want ErrNoRows", err)
	}
}

func TestOutboxStatusCounts(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	_, _ = st.InsertOutbox(ctx, 42, "one", "test")
	_, _ = st.InsertOutbox(ctx, 42, "two", "test")

	counts, err := st.OutboxStatusCounts(ctx)
	if err != nil {
		t.Fatalf("OutboxStatusCounts: %v", err)
	}
	if counts["pending"] != 2 {
		t.Errorf("pending count = %d, want 2", counts["pending"])
	}
}

func TestPoolConfigApplied(t *testing.T) {
	dsn := testdb.DSN(t)
	ctx := context.Background()

	// Non-zero lifetimes: pgx v5.10 treats zero MaxConnIdleTime as "reap
	// immediately", which with MinConns>0 makes the pool churn connections.
	st, err := store.New(ctx, dsn, store.PoolConfig{
		MinConns:        1,
		MaxConns:        3,
		MaxConnLifetime: time.Minute,
		MaxConnIdleTime: time.Minute,
	})
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()
	if got := st.Pool().Stat().MaxConns(); got != 3 {
		t.Errorf("MaxConns = %d, want 3", got)
	}
}
