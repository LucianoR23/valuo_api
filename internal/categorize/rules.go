package categorize

import (
	"cmp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Rule is one rule of the space (plan 3.8) as the matcher reads it.
type Rule struct {
	ID uuid.UUID
	// Key is the text to find, as textnorm.Key.
	Key string
	// Conditions; nil is "any".
	PaymentMethodID *uuid.UUID
	Min, Max        *decimal.Decimal
	// What it fills; at least one is set.
	CategoryID         *uuid.UUID
	SetPaymentMethodID *uuid.UUID
	// SetOwnerID and SetActive tell whether the payment method it fills can
	// be used: only by its owner, and only while active.
	SetOwnerID *uuid.UUID
	SetActive  bool
	// BaseCurrency is the space's currency, the one of Min and Max.
	BaseCurrency string
	CreatedAt    time.Time
}

// Conditions counts the rule's conditions: the payment method and the
// amount range count one each.
func (r Rule) Conditions() int {
	n := 0
	if r.PaymentMethodID != nil {
		n++
	}
	if r.Min != nil || r.Max != nil {
		n++
	}
	return n
}

// Matches tells whether the rule applies to an expense whose description
// is key (textnorm.Key) with what in knows. The text matches whole words:
// "carrefour" finds "carrefour palermo" but not "carrefourcito".
func (r Rule) Matches(key string, in Input) bool {
	if r.Key == "" || !strings.Contains(" "+key+" ", " "+r.Key+" ") {
		return false
	}
	if r.PaymentMethodID != nil && *r.PaymentMethodID != in.PaymentMethodID {
		return false
	}
	if r.Min == nil && r.Max == nil {
		return true
	}
	cur := in.Currency
	if cur == "" {
		cur = r.BaseCurrency
	}
	if !in.Amount.IsPositive() || cur != r.BaseCurrency {
		return false
	}
	if r.Min != nil && in.Amount.LessThan(*r.Min) {
		return false
	}
	return r.Max == nil || !in.Amount.GreaterThan(*r.Max)
}

// matchRules returns the category and the payment method the rules give.
// The rule with more conditions wins, then the longer text, then the
// newest; each field comes from the first rule that can fill it. A payment
// method is only filled when the person did not choose one, and only with
// one of theirs that is active.
func matchRules(rules []Rule, userID uuid.UUID, key string, in Input, known map[uuid.UUID]bool) (uuid.UUID, *uuid.UUID) {
	matching := make([]Rule, 0, len(rules))
	for _, r := range rules {
		if r.Matches(key, in) {
			matching = append(matching, r)
		}
	}
	slices.SortStableFunc(matching, func(a, b Rule) int {
		return cmp.Or(
			cmp.Compare(b.Conditions(), a.Conditions()),
			cmp.Compare(utf8.RuneCountInString(b.Key), utf8.RuneCountInString(a.Key)),
			b.CreatedAt.Compare(a.CreatedAt),
		)
	})
	var cat uuid.UUID
	var pm *uuid.UUID
	for _, r := range matching {
		if cat == uuid.Nil && r.CategoryID != nil && known[*r.CategoryID] {
			cat = *r.CategoryID
		}
		if pm == nil && in.PaymentMethodID == uuid.Nil && r.SetPaymentMethodID != nil &&
			r.SetActive && r.SetOwnerID != nil && *r.SetOwnerID == userID {
			id := *r.SetPaymentMethodID
			pm = &id
		}
	}
	return cat, pm
}
