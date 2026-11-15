package dbmigrate_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/LucianoR23/valuo_api/internal/testutil/testdb"
)

// Migration 000050 (plan 3.8): rules that fill a new expense.

func TestRulesMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbURL := testdb.NewEmptyURL(t)
	m := migrator(t, dbURL)
	if err := m.Migrate(50); err != nil {
		t.Fatalf("up to 50: %v", err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	user := newUser(t, pool)
	space := newSpace(t, pool, user, "personal")
	cat := newCategory(t, pool, space, "Súper", "cart")
	insert := func(key string, minAmount, maxAmount any, category any) error {
		_, err := pool.Exec(ctx, `INSERT INTO rules (household_id, created_by, match_text, match_key, min_amount, max_amount, set_category_id)
			VALUES ($1, $2, $3, $3, $4, $5, $6)`, space, user, key, minAmount, maxAmount, category)
		return err
	}
	if err := insert("carrefour", nil, nil, cat); err != nil {
		t.Fatal(err)
	}
	if err := insert("carrefour", nil, nil, cat); err == nil {
		t.Error("the same rule twice was accepted (NULLS NOT DISTINCT)")
	}
	if err := insert("carrefour", 100, nil, cat); err != nil {
		t.Errorf("same text with a condition: %v", err)
	}
	if err := insert("coto", nil, nil, nil); err == nil {
		t.Error("a rule that fills nothing was accepted")
	}
	if err := insert("dia", 500, 100, cat); err == nil {
		t.Error("min over max was accepted")
	}
	if err := insert("x", nil, nil, cat); err == nil {
		t.Error("a 1-character text was accepted")
	}

	if _, err := pool.Exec(ctx, `DELETE FROM categories WHERE id = $1`, cat); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM rules`).Scan(&n); err != nil || n != 0 {
		t.Errorf("rules left after deleting the category: %d %v", n, err)
	}

	if err := m.Migrate(49); err != nil {
		t.Fatalf("down to 49: %v", err)
	}
	if err := m.Migrate(50); err != nil {
		t.Fatalf("up to 50 again: %v", err)
	}
}
