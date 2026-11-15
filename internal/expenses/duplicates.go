package expenses

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/shopspring/decimal"

	sqlcgen "github.com/LucianoR23/valuo_api/internal/db/sqlc"
	"github.com/LucianoR23/valuo_api/internal/domain"
	"github.com/LucianoR23/valuo_api/internal/money"
	"github.com/LucianoR23/valuo_api/internal/textnorm"
)

// Duplicate detection (plan 3.9, decided with the user): before saving, the
// app and the bot look for an expense of the same space that may be the same
// one. Confirmed expenses match with the same currency, an amount within
// DuplicateTolerance, a date within DuplicateDays and a shared word. A
// pending draft of a variable recurring expense matches with a shared word
// and a date within DraftDays, whatever its amount: the estimate and the real
// bill differ, and loading the bill should complete the draft.
var DuplicateTolerance = decimal.RequireFromString("0.02")

const (
	DuplicateDays = 1
	DraftDays     = 10
	// MaxDuplicates is how many matches are returned.
	MaxDuplicates = 3
	// minWordRunes: shorter words ("de", "y") say nothing.
	minWordRunes = 3
)

// stopWords are words of 3+ letters that two unrelated descriptions often
// share.
var stopWords = map[string]bool{
	"con": true, "del": true, "las": true, "los": true, "por": true, "para": true,
	"una": true, "uno": true, "unos": true, "unas": true, "que": true, "mas": true,
	"sin": true, "pago": true, "compra": true, "gasto": true,
}

// DuplicateInput is what is known of the expense being loaded.
type DuplicateInput struct {
	Description string
	Amount      decimal.Decimal
	Currency    string
	SpentAt     time.Time
}

// Duplicate is an expense that may be the one being loaded.
type Duplicate struct {
	ID            uuid.UUID
	Description   string
	Amount        decimal.Decimal
	Currency      string
	SpentAt       time.Time
	Status        string // "confirmed" or "draft"
	CreatedBy     uuid.UUID
	CreatedByName string
}

// words are the description's meaningful words, normalized.
func words(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(textnorm.Key(s)) {
		if utf8.RuneCountInString(w) >= minWordRunes && !stopWords[w] {
			out[w] = true
		}
	}
	return out
}

// SharesWord reports whether a and b have a meaningful word in common:
// "Carrefour Palermo" and "carrefour" do, "Cena con Ana" and "Taxi con
// Juan" do not.
func SharesWord(a, b string) bool {
	wa := words(a)
	if len(wa) == 0 {
		return false
	}
	for w := range words(b) {
		if wa[w] {
			return true
		}
	}
	return false
}

func pgDate(t time.Time) pgtype.Date {
	return pgtype.Date{Time: time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), Valid: true}
}

// DuplicateCandidates reads the expenses close in amount and date, and the
// drafts close in date (ListDuplicateCandidates).
func (r *Repository) DuplicateCandidates(ctx context.Context, householdID uuid.UUID, in DuplicateInput) ([]Duplicate, error) {
	delta := in.Amount.Mul(DuplicateTolerance)
	rows, err := r.q.ListDuplicateCandidates(ctx, sqlcgen.ListDuplicateCandidatesParams{
		HouseholdID: householdID,
		Currency:    in.Currency,
		MinAmount:   money.ToNumeric(in.Amount.Sub(delta), 2),
		MaxAmount:   money.ToNumeric(in.Amount.Add(delta), 2),
		FromDate:    pgDate(in.SpentAt.AddDate(0, 0, -DuplicateDays)),
		ToDate:      pgDate(in.SpentAt.AddDate(0, 0, DuplicateDays)),
		DraftFrom:   pgDate(in.SpentAt.AddDate(0, 0, -DraftDays)),
		DraftTo:     pgDate(in.SpentAt.AddDate(0, 0, DraftDays)),
	})
	if err != nil {
		return nil, fmt.Errorf("expenses.DuplicateCandidates: %w", err)
	}
	out := make([]Duplicate, len(rows))
	for i, row := range rows {
		out[i] = Duplicate{
			ID: row.ID, Description: row.Description, Amount: money.FromNumeric(row.Amount),
			Currency: row.Currency, SpentAt: row.SpentAt.Time, Status: row.Status,
			CreatedBy: row.CreatedBy, CreatedByName: row.CreatedByName,
		}
	}
	return out, nil
}

// Duplicates lists up to MaxDuplicates expenses of the space that may be the
// one described: pending drafts first, then the newest.
func (s *Service) Duplicates(ctx context.Context, householdID uuid.UUID, in DuplicateInput) ([]Duplicate, error) {
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	if !in.Amount.IsPositive() {
		return nil, domain.NewValidationError("amount", "debe ser mayor a 0")
	}
	if len(in.Currency) != 3 {
		return nil, domain.NewValidationError("currency", "código de moneda inválido")
	}
	if in.SpentAt.IsZero() {
		return nil, domain.NewValidationError("spentAt", "requerido")
	}
	if len(words(in.Description)) == 0 {
		return []Duplicate{}, nil
	}
	rows, err := s.repo.DuplicateCandidates(ctx, householdID, in)
	if err != nil {
		return nil, err
	}
	out := []Duplicate{}
	for _, d := range rows {
		if SharesWord(in.Description, d.Description) {
			out = append(out, d)
			if len(out) == MaxDuplicates {
				break
			}
		}
	}
	return out, nil
}
