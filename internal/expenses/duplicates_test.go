package expenses_test

// Duplicate detection (plan 3.9): POST /expenses/duplicates.

import (
	"net/http"
	"testing"

	"github.com/LucianoR23/valuo_api/internal/expenses"
	"github.com/LucianoR23/valuo_api/internal/testutil/apitest"
)

func TestSharesWord(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a, b string
		want bool
	}{
		{"Carrefour Palermo", "carrefour", true},
		{"Súper Día", "super", true},
		{"Pingüino", "PINGUINO!", true},
		{"Cena con Ana", "Taxi con Juan", false}, // "con" says nothing
		{"Luz de", "Gas de", false},              // short words neither
		{"Carrefour", "Carrefourcito", false},    // whole words only
		{"", "Carrefour", false},
		{"Pago Edenor", "Edenor luz", true},
	}
	for _, c := range cases {
		if got := expenses.SharesWord(c.a, c.b); got != c.want {
			t.Errorf("SharesWord(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

type dupDTO struct {
	ID            string  `json:"id"`
	Description   string  `json:"description"`
	Amount        float64 `json:"amount"`
	Currency      string  `json:"currency"`
	SpentAt       string  `json:"spentAt"`
	Status        string  `json:"status"`
	CreatedBy     string  `json:"createdBy"`
	CreatedByName string  `json:"createdByName"`
}

func (h house) duplicates(t *testing.T, u apitest.User, body map[string]any) []dupDTO {
	t.Helper()
	var out []dupDTO
	h.do(t, u, http.MethodPost, "/expenses/duplicates", body).Status(t, http.StatusOK).JSON(t, &out)
	return out
}

func TestDuplicates(t *testing.T) {
	t.Parallel()
	s := apitest.New(t)

	t.Run("close amount, date and a shared word, from any member", func(t *testing.T) {
		h := newHouse(t, s)
		e := h.cashExpense(t, h.member, 12500, map[string]any{"description": "Carrefour Palermo", "spentAt": "2026-11-14"}).Expense
		h.cashExpense(t, h.owner, 12500, map[string]any{"description": "Farmacia", "spentAt": "2026-11-14"})  // other words
		h.cashExpense(t, h.owner, 12800, map[string]any{"description": "Carrefour", "spentAt": "2026-11-14"}) // over 2%
		h.cashExpense(t, h.owner, 12500, map[string]any{"description": "Carrefour", "spentAt": "2026-11-12"}) // 3 days off
		usd := h.cashExpense(t, h.owner, 12500, map[string]any{"description": "Carrefour", "spentAt": "2026-11-14"}).Expense
		if _, err := s.Pool.Exec(t.Context(), `UPDATE expenses SET currency = 'USD' WHERE id = $1`, usd.ID); err != nil {
			t.Fatal(err)
		}

		got := h.duplicates(t, h.owner, map[string]any{"description": "carrefour", "amount": 12300, "currency": "ars", "spentAt": "2026-11-15"})
		if len(got) != 1 {
			t.Fatalf("duplicates = %+v", got)
		}
		d := got[0]
		if d.ID != e.ID || d.Status != "confirmed" || d.Amount != 12500 || d.SpentAt != "2026-11-14" ||
			d.CreatedBy != h.member.ID || d.CreatedByName != "Miembro" {
			t.Errorf("duplicate = %+v", d)
		}
	})

	t.Run("a pending bill matches by word and date, with any amount, first", func(t *testing.T) {
		h := newHouse(t, s)
		draft := h.cashExpense(t, h.owner, 30000, map[string]any{"description": "Luz", "spentAt": "2026-11-06"}).Expense
		if _, err := s.Pool.Exec(t.Context(), `UPDATE expenses SET status = 'draft' WHERE id = $1`, draft.ID); err != nil {
			t.Fatal(err)
		}
		old := h.cashExpense(t, h.owner, 30000, map[string]any{"description": "Luz", "spentAt": "2026-10-20"}).Expense
		if _, err := s.Pool.Exec(t.Context(), `UPDATE expenses SET status = 'draft' WHERE id = $1`, old.ID); err != nil {
			t.Fatal(err)
		}
		same := h.cashExpense(t, h.owner, 32400, map[string]any{"description": "Luz Edenor", "spentAt": "2026-11-15"}).Expense

		got := h.duplicates(t, h.owner, map[string]any{"description": "Edenor luz", "amount": 32400, "currency": "ARS", "spentAt": "2026-11-15"})
		if len(got) != 2 || got[0].ID != draft.ID || got[0].Status != "draft" || got[1].ID != same.ID {
			t.Errorf("duplicates = %+v", got)
		}
	})

	t.Run("each space sees only its own expenses", func(t *testing.T) {
		h := newHouse(t, s)
		h.cashExpense(t, h.owner, 5000, map[string]any{"description": "Uber", "spentAt": "2026-11-15"})
		other := newHouse(t, s)
		if got := other.duplicates(t, other.owner, map[string]any{"description": "Uber", "amount": 5000, "currency": "ARS", "spentAt": "2026-11-15"}); len(got) != 0 {
			t.Errorf("another space's duplicates = %+v", got)
		}
		outsider := s.Register(t, apitest.RegisterOpts{})
		h.do(t, outsider, http.MethodPost, "/expenses/duplicates", map[string]any{
			"description": "Uber", "amount": 5000, "currency": "ARS", "spentAt": "2026-11-15",
		}).Status(t, http.StatusNotFound)
	})

	t.Run("a description without meaningful words finds nothing", func(t *testing.T) {
		h := newHouse(t, s)
		h.cashExpense(t, h.owner, 5000, map[string]any{"description": "de", "spentAt": "2026-11-15"})
		if got := h.duplicates(t, h.owner, map[string]any{"description": "de", "amount": 5000, "currency": "ARS", "spentAt": "2026-11-15"}); len(got) != 0 {
			t.Errorf("duplicates = %+v", got)
		}
	})

	t.Run("validation", func(t *testing.T) {
		h := newHouse(t, s)
		for field, body := range map[string]map[string]any{
			"amount":   {"description": "Uber", "amount": 0, "currency": "ARS", "spentAt": "2026-11-15"},
			"currency": {"description": "Uber", "amount": 10, "currency": "PESOS", "spentAt": "2026-11-15"},
			"spentAt":  {"description": "Uber", "amount": 10, "currency": "ARS", "spentAt": "15/11"},
		} {
			h.do(t, h.owner, http.MethodPost, "/expenses/duplicates", body).WantError(t, http.StatusUnprocessableEntity, "validation", field)
		}
	})
}
