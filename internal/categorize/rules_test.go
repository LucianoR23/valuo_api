package categorize

// Rules of plan 3.8: matching, precedence and their place in the chain.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/LucianoR23/valuo_api/internal/ai"
	"github.com/LucianoR23/valuo_api/internal/textnorm"
)

func ptr[T any](v T) *T { return &v }

func dec(s string) *decimal.Decimal {
	d := decimal.RequireFromString(s)
	return &d
}

var t0 = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

func TestRuleMatches(t *testing.T) {
	t.Parallel()
	visa := uuid.New()
	cases := []struct {
		name string
		rule Rule
		desc string
		in   Input
		want bool
	}{
		{"whole word", Rule{Key: "carrefour"}, "Carrefour Palermo", Input{}, true},
		{"accents and case", Rule{Key: "pinguino"}, "PINGÜINO!", Input{}, true},
		{"several words", Rule{Key: "uber eats"}, "uber eats pizza", Input{}, true},
		{"not part of a word", Rule{Key: "carrefour"}, "Carrefourcito", Input{}, false},
		{"words out of order", Rule{Key: "uber eats"}, "eats uber", Input{}, false},
		{"payment method required", Rule{Key: "uber", PaymentMethodID: &visa}, "Uber", Input{}, false},
		{"payment method given", Rule{Key: "uber", PaymentMethodID: &visa}, "Uber", Input{PaymentMethodID: visa}, true},
		{"amount unknown", Rule{Key: "uber", Min: dec("20000"), BaseCurrency: "ARS"}, "Uber", Input{}, false},
		{"amount at min", Rule{Key: "uber", Min: dec("20000"), BaseCurrency: "ARS"}, "Uber", Input{Amount: *dec("20000")}, true},
		{"amount under min", Rule{Key: "uber", Min: dec("20000"), BaseCurrency: "ARS"}, "Uber", Input{Amount: *dec("19999.99")}, false},
		{"amount at max", Rule{Key: "uber", Max: dec("5000"), BaseCurrency: "ARS"}, "Uber", Input{Amount: *dec("5000"), Currency: "ARS"}, true},
		{"amount over max", Rule{Key: "uber", Max: dec("5000"), BaseCurrency: "ARS"}, "Uber", Input{Amount: *dec("5000.01")}, false},
		{"other currency", Rule{Key: "uber", Min: dec("10"), BaseCurrency: "ARS"}, "Uber", Input{Amount: *dec("50"), Currency: "USD"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			key := textnorm.Key(c.desc)
			if got := c.rule.Matches(key, c.in); got != c.want {
				t.Errorf("Matches(%q) = %v, want %v", key, got, c.want)
			}
		})
	}
}

func TestMatchRulesPrecedence(t *testing.T) {
	t.Parallel()
	visa, cash, other := uuid.New(), uuid.New(), uuid.New()
	known := map[uuid.UUID]bool{super.ID: true, transport.ID: true, health.ID: true}
	plain := Rule{Key: "uber", CategoryID: &transport.ID, CreatedAt: t0}
	longer := Rule{Key: "uber eats", CategoryID: &super.ID, CreatedAt: t0}
	withPM := Rule{Key: "uber", PaymentMethodID: &visa, CategoryID: &health.ID, CreatedAt: t0}
	newer := Rule{Key: "uber", CategoryID: &super.ID, CreatedAt: t0.Add(time.Hour)}

	cat, _ := matchRules([]Rule{plain, longer}, user, "uber eats pizza", Input{}, known)
	if cat != super.ID {
		t.Errorf("longer text should win, got %v", cat)
	}
	cat, _ = matchRules([]Rule{longer, withPM}, user, "uber eats", Input{PaymentMethodID: visa}, known)
	if cat != health.ID {
		t.Errorf("more conditions should win, got %v", cat)
	}
	cat, _ = matchRules([]Rule{plain, newer}, user, "uber", Input{}, known)
	if cat != super.ID {
		t.Errorf("newest should win a tie, got %v", cat)
	}
	gone := Rule{Key: "uber", CategoryID: ptr(uuid.New()), CreatedAt: t0.Add(2 * time.Hour)}
	cat, _ = matchRules([]Rule{gone, plain}, user, "uber", Input{}, known)
	if cat != transport.ID {
		t.Errorf("a category of another space is skipped, got %v", cat)
	}

	// Payment method: only the owner's, only active, never over a chosen one.
	fills := Rule{Key: "uber", SetPaymentMethodID: &cash, SetOwnerID: &user, SetActive: true, CreatedAt: t0}
	if _, pm := matchRules([]Rule{fills}, user, "uber", Input{}, known); pm == nil || *pm != cash {
		t.Errorf("pm %v", pm)
	}
	if _, pm := matchRules([]Rule{fills}, user, "uber", Input{PaymentMethodID: other}, known); pm != nil {
		t.Errorf("a chosen payment method is kept, got %v", *pm)
	}
	if _, pm := matchRules([]Rule{fills}, uuid.New(), "uber", Input{}, known); pm != nil {
		t.Errorf("another member's payment method is not used, got %v", *pm)
	}
	inactive := fills
	inactive.SetActive = false
	if _, pm := matchRules([]Rule{inactive}, user, "uber", Input{}, known); pm != nil {
		t.Errorf("an inactive payment method is not used, got %v", *pm)
	}
	// Each field comes from the first rule that can fill it.
	cat, pm := matchRules([]Rule{withPM, fills}, user, "uber", Input{}, known)
	if cat != uuid.Nil || pm == nil || *pm != cash {
		t.Errorf("cat %v pm %v", cat, pm)
	}
}

func TestRulesComeFirstInTheChain(t *testing.T) {
	t.Parallel()
	cash := uuid.New()
	f := &fakeAI{enabled: true, answer: `{"match":true,"category":"Salud"}`}
	st := &fakeStore{cats: cats, past: []Past{{"Carrefour", transport.ID}}, rules: []Rule{
		{Key: "carrefour", CategoryID: &super.ID, CreatedAt: t0},
		{Key: "kiosco", SetPaymentMethodID: &cash, SetOwnerID: &user, SetActive: true, CreatedAt: t0},
	}}
	s := New(st, f, nil)
	ctx := context.Background()

	got, ok, err := s.SuggestFor(ctx, user, hh, Input{Description: "Carrefour Palermo"})
	if err != nil || !ok || got.CategoryID != super.ID || got.Source != SourceRule || len(f.got) != 0 {
		t.Fatalf("rule: %+v %v %v, ai calls %d", got, ok, err, len(f.got))
	}

	// A rule that only fills the payment method: the category comes from
	// the AI, recorded under the channel asked for.
	got, ok, err = s.SuggestFor(ctx, user, hh, Input{Description: "Kiosco de la esquina", Channel: ai.ChannelTelegram})
	if err != nil || !ok || got.CategoryID != health.ID || got.Source != SourceAI || got.PaymentMethodID == nil || *got.PaymentMethodID != cash {
		t.Fatalf("pm rule: %+v %v %v", got, ok, err)
	}
	if len(f.got) != 1 || f.got[0].Channel != ai.ChannelTelegram {
		t.Errorf("ai requests %+v", f.got)
	}

	// Suggest is only about the category.
	f.answer = `{"match":false,"category":"Salud"}`
	if got, ok := suggest(t, st, f, "Kiosco"); ok {
		t.Errorf("Suggest returned a payment-method-only rule: %+v", got)
	}
	if got, ok, _ := s.SuggestFor(ctx, user, hh, Input{Description: "Kiosco"}); !ok || got.CategoryID != uuid.Nil || got.PaymentMethodID == nil {
		t.Errorf("SuggestFor: %+v %v", got, ok)
	}
}
