package categorize

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	sqlcgen "github.com/LucianoR23/valuo_api/internal/db/sqlc"
	"github.com/LucianoR23/valuo_api/internal/money"
)

// PGStore reads categories and history from Postgres.
type PGStore struct {
	q *sqlcgen.Queries
}

// NewStore builds the Postgres store.
func NewStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{q: sqlcgen.New(pool)}
}

func (s *PGStore) Categories(ctx context.Context, householdID uuid.UUID) ([]Category, error) {
	rows, err := s.q.ListCategoriesByHousehold(ctx, householdID)
	if err != nil {
		return nil, fmt.Errorf("categorize: categories: %w", err)
	}
	out := make([]Category, len(rows))
	for i, r := range rows {
		out[i] = Category{ID: r.ID, Name: r.Name}
	}
	return out, nil
}

func (s *PGStore) History(ctx context.Context, householdID uuid.UUID) ([]Past, error) {
	rows, err := s.q.ListCategorizedHistory(ctx, householdID)
	if err != nil {
		return nil, fmt.Errorf("categorize: history: %w", err)
	}
	out := make([]Past, 0, len(rows))
	for _, r := range rows {
		if r.CategoryID != nil {
			out = append(out, Past{Description: r.Description, CategoryID: *r.CategoryID})
		}
	}
	return out, nil
}

func (s *PGStore) Rules(ctx context.Context, householdID uuid.UUID) ([]Rule, error) {
	rows, err := s.q.ListRulesForMatch(ctx, householdID)
	if err != nil {
		return nil, fmt.Errorf("categorize: rules: %w", err)
	}
	out := make([]Rule, len(rows))
	for i, r := range rows {
		out[i] = Rule{
			ID: r.ID, Key: r.MatchKey, PaymentMethodID: r.PaymentMethodID,
			Min: decimalPtr(r.MinAmount), Max: decimalPtr(r.MaxAmount),
			CategoryID: r.SetCategoryID, SetPaymentMethodID: r.SetPaymentMethodID,
			SetOwnerID: r.SetOwnerID, SetActive: r.SetActive,
			BaseCurrency: r.BaseCurrency, CreatedAt: r.CreatedAt.Time,
		}
	}
	return out, nil
}

// decimalPtr is nil for a NULL numeric.
func decimalPtr(n pgtype.Numeric) *decimal.Decimal {
	if !n.Valid {
		return nil
	}
	d := money.FromNumeric(n)
	return &d
}
