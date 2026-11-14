// Package rules keeps the rules that fill a new expense (plan 3.8): "when
// the description says Carrefour, category Súper". A rule belongs to a
// space; any member can see and change it. The matching itself lives in
// internal/categorize, first in the suggestion chain. The package also
// lists the space's recent descriptions for autocomplete in Cargar gasto.
package rules

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	sqlcgen "github.com/LucianoR23/valuo_api/internal/db/sqlc"
	"github.com/LucianoR23/valuo_api/internal/domain"
	"github.com/LucianoR23/valuo_api/internal/money"
	"github.com/LucianoR23/valuo_api/internal/textnorm"
)

const (
	// MaxTextRunes caps the text a rule looks for.
	MaxTextRunes = 60
	// maxAmount fits NUMERIC(14,2).
	maxAmount = 999_999_999_999
	// descriptionsMonths is how far back autocomplete looks.
	descriptionsMonths = 12
	// maxDescriptions is how many descriptions autocomplete gets.
	maxDescriptions = 200
)

// Rule is a rule of a space.
type Rule struct {
	ID                 uuid.UUID
	HouseholdID        uuid.UUID
	CreatedBy          uuid.UUID
	MatchText          string
	PaymentMethodID    *uuid.UUID
	MinAmount          *decimal.Decimal
	MaxAmount          *decimal.Decimal
	SetCategoryID      *uuid.UUID
	SetPaymentMethodID *uuid.UUID
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// Input is a new rule, or the whole rule on an update.
type Input struct {
	MatchText          string
	PaymentMethodID    *uuid.UUID
	MinAmount          *decimal.Decimal
	MaxAmount          *decimal.Decimal
	SetCategoryID      *uuid.UUID
	SetPaymentMethodID *uuid.UUID
}

// Description is one description of the space's expenses and how many
// times it was used.
type Description struct {
	Text string
	Uses int
}

// Service validates and stores rules.
type Service struct {
	q        *sqlcgen.Queries
	maxRules int
	now      func() time.Time
	loc      *time.Location
}

// NewService builds the service. maxRules caps the rules of a space
// (RULES_MAX_PER_SPACE).
func NewService(pool *pgxpool.Pool, maxRules int, loc *time.Location, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	if loc == nil {
		loc = time.UTC
	}
	return &Service{q: sqlcgen.New(pool), maxRules: maxRules, now: now, loc: loc}
}

// List returns the space's rules, by text.
func (s *Service) List(ctx context.Context, householdID uuid.UUID) ([]Rule, error) {
	rows, err := s.q.ListRules(ctx, householdID)
	if err != nil {
		return nil, fmt.Errorf("rules: list: %w", err)
	}
	out := make([]Rule, len(rows))
	for i, r := range rows {
		out[i] = toRule(r)
	}
	return out, nil
}

// Create adds a rule to the space. Payment methods must be the caller's.
func (s *Service) Create(ctx context.Context, userID, householdID uuid.UUID, in Input) (Rule, error) {
	text, key, err := s.validate(ctx, userID, householdID, in, nil)
	if err != nil {
		return Rule{}, err
	}
	n, err := s.q.CountRules(ctx, householdID)
	if err != nil {
		return Rule{}, fmt.Errorf("rules: count: %w", err)
	}
	if n >= int64(s.maxRules) {
		return Rule{}, domain.NewConflictError(fmt.Sprintf("Llegaste al máximo de %d reglas en este espacio. Borrá alguna para crear otra.", s.maxRules))
	}
	row, err := s.q.CreateRule(ctx, sqlcgen.CreateRuleParams{
		HouseholdID: householdID, CreatedBy: userID, MatchText: text, MatchKey: key,
		PaymentMethodID: in.PaymentMethodID, MinAmount: numeric(in.MinAmount), MaxAmount: numeric(in.MaxAmount),
		SetCategoryID: in.SetCategoryID, SetPaymentMethodID: in.SetPaymentMethodID,
	})
	if err != nil {
		return Rule{}, mapErr(err)
	}
	return toRule(row), nil
}

// Update replaces a rule. A payment method it already had can stay even
// when it is another member's; a new one must be the caller's.
func (s *Service) Update(ctx context.Context, userID, householdID, id uuid.UUID, in Input) (Rule, error) {
	cur, err := s.q.GetRule(ctx, sqlcgen.GetRuleParams{ID: id, HouseholdID: householdID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Rule{}, domain.ErrNotFound
	}
	if err != nil {
		return Rule{}, fmt.Errorf("rules: get: %w", err)
	}
	text, key, err := s.validate(ctx, userID, householdID, in, &cur)
	if err != nil {
		return Rule{}, err
	}
	row, err := s.q.UpdateRule(ctx, sqlcgen.UpdateRuleParams{
		ID: id, HouseholdID: householdID, MatchText: text, MatchKey: key,
		PaymentMethodID: in.PaymentMethodID, MinAmount: numeric(in.MinAmount), MaxAmount: numeric(in.MaxAmount),
		SetCategoryID: in.SetCategoryID, SetPaymentMethodID: in.SetPaymentMethodID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Rule{}, domain.ErrNotFound
	}
	if err != nil {
		return Rule{}, mapErr(err)
	}
	return toRule(row), nil
}

// Delete removes a rule of the space.
func (s *Service) Delete(ctx context.Context, householdID, id uuid.UUID) error {
	n, err := s.q.DeleteRule(ctx, sqlcgen.DeleteRuleParams{ID: id, HouseholdID: householdID})
	if err != nil {
		return fmt.Errorf("rules: delete: %w", err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// Descriptions returns the descriptions of the space's expenses of the
// last 12 months, most used first. Spellings of the same words ("Súper",
// "super") count as one, shown as the most used.
func (s *Service) Descriptions(ctx context.Context, householdID uuid.UUID) ([]Description, error) {
	y, m, d := s.now().In(s.loc).Date()
	since := time.Date(y, m, d, 0, 0, 0, 0, time.UTC).AddDate(0, -descriptionsMonths, 0)
	rows, err := s.q.ListRecentDescriptions(ctx, sqlcgen.ListRecentDescriptionsParams{
		HouseholdID: householdID, SpentAt: pgtype.Date{Time: since, Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("rules: descriptions: %w", err)
	}
	type group struct {
		Description
		best  int
		order int
	}
	byKey := map[string]*group{}
	for i, r := range rows {
		text := strings.TrimSpace(r.Description)
		k := textnorm.Key(text)
		if k == "" {
			continue
		}
		g, ok := byKey[k]
		if !ok {
			byKey[k] = &group{Description: Description{Text: text, Uses: int(r.Uses)}, best: int(r.Uses), order: i}
			continue
		}
		g.Uses += int(r.Uses)
		if int(r.Uses) > g.best {
			g.Text, g.best = text, int(r.Uses)
		}
	}
	groups := make([]*group, 0, len(byKey))
	for _, g := range byKey {
		groups = append(groups, g)
	}
	slices.SortFunc(groups, func(a, b *group) int {
		if a.Uses != b.Uses {
			return b.Uses - a.Uses
		}
		return a.order - b.order
	})
	if len(groups) > maxDescriptions {
		groups = groups[:maxDescriptions]
	}
	out := make([]Description, len(groups))
	for i, g := range groups {
		out[i] = g.Description
	}
	return out, nil
}

// validate checks a rule and returns its text and key. cur is the rule
// being replaced, nil on create.
func (s *Service) validate(ctx context.Context, userID, householdID uuid.UUID, in Input, cur *sqlcgen.Rule) (string, string, error) {
	text := strings.Join(strings.Fields(in.MatchText), " ")
	key := textnorm.Key(text)
	switch {
	case utf8.RuneCountInString(text) > MaxTextRunes:
		return "", "", domain.NewValidationError("matchText", fmt.Sprintf("usá hasta %d caracteres", MaxTextRunes))
	case utf8.RuneCountInString(key) < 2 || utf8.RuneCountInString(key) > MaxTextRunes:
		return "", "", domain.NewValidationError("matchText", "escribí al menos 2 letras o números")
	case in.SetCategoryID == nil && in.SetPaymentMethodID == nil:
		return "", "", domain.NewValidationError("setCategoryId", "elegí una categoría o un medio de pago")
	}
	for _, a := range []struct {
		field string
		v     *decimal.Decimal
	}{{"minAmount", in.MinAmount}, {"maxAmount", in.MaxAmount}} {
		if a.v != nil && (!a.v.IsPositive() || a.v.GreaterThan(decimal.NewFromInt(maxAmount))) {
			return "", "", domain.NewValidationError(a.field, "tiene que ser un monto mayor a 0")
		}
	}
	if in.MinAmount != nil && in.MaxAmount != nil && in.MinAmount.GreaterThan(*in.MaxAmount) {
		return "", "", domain.NewValidationError("maxAmount", "tiene que ser mayor o igual al mínimo")
	}
	if in.SetCategoryID != nil {
		ok, err := s.q.CategoryInHousehold(ctx, sqlcgen.CategoryInHouseholdParams{ID: *in.SetCategoryID, HouseholdID: householdID})
		if err != nil {
			return "", "", fmt.Errorf("rules: category: %w", err)
		}
		if !ok {
			return "", "", domain.NewValidationError("setCategoryId", "no es una categoría de este espacio")
		}
	}
	var kept []*uuid.UUID
	if cur != nil {
		kept = []*uuid.UUID{cur.PaymentMethodID, cur.SetPaymentMethodID}
	}
	for _, pm := range []struct {
		field string
		id    *uuid.UUID
	}{{"paymentMethodId", in.PaymentMethodID}, {"setPaymentMethodId", in.SetPaymentMethodID}} {
		if pm.id == nil || slices.ContainsFunc(kept, func(k *uuid.UUID) bool { return k != nil && *k == *pm.id }) {
			continue
		}
		m, err := s.q.GetPaymentMethodByID(ctx, *pm.id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (m.OwnerUserID != userID || !m.IsActive)) {
			return "", "", domain.NewValidationError(pm.field, "no es uno de tus medios de pago")
		}
		if err != nil {
			return "", "", fmt.Errorf("rules: payment method: %w", err)
		}
	}
	return text, key, nil
}

func mapErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return domain.NewConflictError("Ya hay una regla igual en este espacio.")
		case "23503":
			return domain.NewValidationError("rule", "una categoría o un medio de pago ya no existe")
		}
	}
	return fmt.Errorf("rules: save: %w", err)
}

func toRule(r sqlcgen.Rule) Rule {
	return Rule{
		ID: r.ID, HouseholdID: r.HouseholdID, CreatedBy: r.CreatedBy, MatchText: r.MatchText,
		PaymentMethodID: r.PaymentMethodID, MinAmount: decimalPtr(r.MinAmount), MaxAmount: decimalPtr(r.MaxAmount),
		SetCategoryID: r.SetCategoryID, SetPaymentMethodID: r.SetPaymentMethodID,
		CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}
}

func numeric(d *decimal.Decimal) pgtype.Numeric {
	if d == nil {
		return pgtype.Numeric{}
	}
	return money.ToNumeric(*d, 2)
}

func decimalPtr(n pgtype.Numeric) *decimal.Decimal {
	if !n.Valid {
		return nil
	}
	d := money.FromNumeric(n)
	return &d
}
