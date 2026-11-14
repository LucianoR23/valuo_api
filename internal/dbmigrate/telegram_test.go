package dbmigrate_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/LucianoR23/valuo_api/internal/testutil/testdb"
)

// Migration 000049 (plan 3.6): Telegram bot tables and two new ai_usage
// statuses, mapped back to old ones on the way down.

func TestTelegramMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbURL := testdb.NewEmptyURL(t)
	m := migrator(t, dbURL)
	if err := m.Migrate(49); err != nil {
		t.Fatalf("up to 49: %v", err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	user := newUser(t, pool)
	usage := func(status string) error {
		_, err := pool.Exec(ctx, `INSERT INTO ai_usage (user_id, task, channel, status) VALUES ($1, 'parse_expense', 'telegram', $2)`, user, status)
		return err
	}
	for _, s := range []string{"ok", "off_topic", "suspended"} {
		if err := usage(s); err != nil {
			t.Fatalf("status %s: %v", s, err)
		}
	}
	if usage("nonsense") == nil {
		t.Error("unknown status accepted")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO telegram_links (user_id, chat_id) VALUES ($1, 42)`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO telegram_link_codes (code_hash, user_id, expires_at) VALUES ('short', $1, NOW())`, user); err == nil {
		t.Error("a code hash that is not SHA-256 was accepted")
	}

	t.Run("down and up again", func(t *testing.T) {
		if err := m.Migrate(48); err != nil {
			t.Fatalf("down to 48: %v", err)
		}
		var odd int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM ai_usage WHERE status NOT IN ('ok', 'disabled')`).Scan(&odd); err != nil || odd != 0 {
			t.Errorf("statuses left: %d %v", odd, err)
		}
		if err := m.Migrate(49); err != nil {
			t.Fatalf("up to 49: %v", err)
		}
	})
}
