package rules_test

// Rules and autocomplete (plan 3.8) through the API.

import (
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/LucianoR23/valuo_api/internal/config"
	"github.com/LucianoR23/valuo_api/internal/testutil/apitest"
	"github.com/LucianoR23/valuo_api/internal/testutil/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

type ruleJSON struct {
	ID                 string   `json:"id"`
	HouseholdID        string   `json:"householdId"`
	CreatedBy          string   `json:"createdBy"`
	MatchText          string   `json:"matchText"`
	PaymentMethodID    *string  `json:"paymentMethodId"`
	MinAmount          *float64 `json:"minAmount"`
	MaxAmount          *float64 `json:"maxAmount"`
	SetCategoryID      *string  `json:"setCategoryId"`
	SetPaymentMethodID *string  `json:"setPaymentMethodId"`
}

type parsed struct {
	PaymentMethodID *string `json:"paymentMethodId"`
	CategoryID      *string `json:"categoryId"`
	CategorySource  *string `json:"categorySource"`
}

func categories(t *testing.T, s *apitest.Server, u apitest.User, hID string) map[string]string {
	t.Helper()
	var list []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	s.Do(t, apitest.Req{Method: http.MethodGet, Path: "/categories", Token: u.Token, Household: hID}).
		Status(t, http.StatusOK).JSON(t, &list)
	out := map[string]string{}
	for _, c := range list {
		out[c.Name] = c.ID
	}
	return out
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func TestRules(t *testing.T) {
	t.Parallel()
	s := apitest.New(t, apitest.WithConfig(func(c *config.Config) { c.RulesMaxPerSpace = 3 }))
	owner := s.Register(t, apitest.RegisterOpts{FirstName: "Owner", Verify: true})
	h := s.CreateHousehold(t, owner, "")
	member := s.AddMember(t, owner, h.ID)
	cats := categories(t, s, owner, h.ID)
	super, transport := cats["Súper"], cats["Taxi y apps"]
	if super == "" || transport == "" {
		t.Fatalf("default categories %v", cats)
	}
	visa := s.CreateCreditCard(t, owner, "Visa Galicia", 20, 5, "2026-11-20", "2026-12-05")
	memberCash := s.Cash(t, member)

	do := func(u apitest.User, method, path string, body any) *apitest.Res {
		return s.Do(t, apitest.Req{Method: method, Path: path, Token: u.Token, Household: h.ID, Body: body})
	}

	var rule ruleJSON
	do(owner, http.MethodPost, "/rules", map[string]any{
		"matchText": "  Carrefour  ", "setCategoryId": super, "setPaymentMethodId": visa.ID,
	}).Status(t, http.StatusCreated).JSON(t, &rule)
	if rule.MatchText != "Carrefour" || deref(rule.SetCategoryID) != super || deref(rule.SetPaymentMethodID) != visa.ID ||
		rule.HouseholdID != h.ID || rule.CreatedBy != owner.ID {
		t.Fatalf("created %+v", rule)
	}

	t.Run("validation", func(t *testing.T) {
		do(owner, http.MethodPost, "/rules", map[string]any{"matchText": "carrefour", "setCategoryId": super}).
			WantError(t, http.StatusConflict, "conflict", "")
		do(owner, http.MethodPost, "/rules", map[string]any{"matchText": "!", "setCategoryId": super}).
			WantError(t, http.StatusUnprocessableEntity, "validation", "matchText")
		do(owner, http.MethodPost, "/rules", map[string]any{"matchText": "Coto"}).
			WantError(t, http.StatusUnprocessableEntity, "validation", "setCategoryId")
		do(owner, http.MethodPost, "/rules", map[string]any{"matchText": "Coto", "setCategoryId": super, "minAmount": 500, "maxAmount": 100}).
			WantError(t, http.StatusUnprocessableEntity, "validation", "maxAmount")
		do(owner, http.MethodPost, "/rules", map[string]any{"matchText": "Coto", "setCategoryId": super, "minAmount": -1}).
			WantError(t, http.StatusUnprocessableEntity, "validation", "minAmount")
		other := categories(t, s, owner, s.PersonalSpace(t, owner))["Súper"]
		do(owner, http.MethodPost, "/rules", map[string]any{"matchText": "Coto", "setCategoryId": other}).
			WantError(t, http.StatusUnprocessableEntity, "validation", "setCategoryId")
		do(owner, http.MethodPost, "/rules", map[string]any{"matchText": "Coto", "setPaymentMethodId": memberCash.ID}).
			WantError(t, http.StatusUnprocessableEntity, "validation", "setPaymentMethodId")
		do(owner, http.MethodPost, "/rules", map[string]any{"matchText": "Coto", "setCategoryId": super, "paymentMethodId": memberCash.ID}).
			WantError(t, http.StatusUnprocessableEntity, "validation", "paymentMethodId")
	})

	t.Run("a member edits a rule and keeps the owner's payment method", func(t *testing.T) {
		var got ruleJSON
		do(member, http.MethodPatch, "/rules/"+rule.ID, map[string]any{
			"matchText": "Carrefour", "setCategoryId": transport, "setPaymentMethodId": visa.ID, "minAmount": 1000,
		}).Status(t, http.StatusOK).JSON(t, &got)
		if deref(got.SetCategoryID) != transport || got.MinAmount == nil || *got.MinAmount != 1000 {
			t.Fatalf("updated %+v", got)
		}
		do(member, http.MethodPatch, "/rules/"+rule.ID, map[string]any{"matchText": "Carrefour", "setCategoryId": super, "setPaymentMethodId": visa.ID}).
			Status(t, http.StatusOK)
	})

	t.Run("the rule fills the sentence and the suggestion", func(t *testing.T) {
		var p parsed
		do(owner, http.MethodPost, "/expenses/parse", map[string]any{"text": "carrefour palermo 15000"}).
			Status(t, http.StatusOK).JSON(t, &p)
		if deref(p.CategoryID) != super || deref(p.CategorySource) != "rule" || deref(p.PaymentMethodID) != visa.ID {
			t.Errorf("owner parse %+v", p)
		}
		// The owner's card is not the member's: only the category.
		p = parsed{}
		do(member, http.MethodPost, "/expenses/parse", map[string]any{"text": "carrefour 15000"}).
			Status(t, http.StatusOK).JSON(t, &p)
		if deref(p.CategoryID) != super || p.PaymentMethodID != nil {
			t.Errorf("member parse %+v", p)
		}
		var sg struct {
			CategoryID *string `json:"categoryId"`
			Source     *string `json:"source"`
		}
		do(member, http.MethodPost, "/categories/suggest", map[string]any{"description": "Carrefour Express"}).
			Status(t, http.StatusOK).JSON(t, &sg)
		if deref(sg.CategoryID) != super || deref(sg.Source) != "rule" {
			t.Errorf("suggest %+v", sg)
		}
	})

	t.Run("limit per space", func(t *testing.T) {
		do(owner, http.MethodPost, "/rules", map[string]any{"matchText": "Uber", "setCategoryId": transport}).Status(t, http.StatusCreated)
		do(owner, http.MethodPost, "/rules", map[string]any{"matchText": "Cabify", "setCategoryId": transport}).Status(t, http.StatusCreated)
		do(owner, http.MethodPost, "/rules", map[string]any{"matchText": "Didi", "setCategoryId": transport}).
			WantError(t, http.StatusConflict, "conflict", "")
	})

	t.Run("deleting the category deletes its rules", func(t *testing.T) {
		do(owner, http.MethodDelete, "/categories/"+transport, nil).Status(t, http.StatusNoContent)
		var list []ruleJSON
		do(member, http.MethodGet, "/rules", nil).Status(t, http.StatusOK).JSON(t, &list)
		if len(list) != 1 || list[0].ID != rule.ID {
			t.Errorf("rules %+v", list)
		}
	})

	t.Run("delete", func(t *testing.T) {
		do(member, http.MethodDelete, "/rules/"+rule.ID, nil).Status(t, http.StatusNoContent)
		do(member, http.MethodDelete, "/rules/"+rule.ID, nil).WantError(t, http.StatusNotFound, "not_found", "")
		stranger := s.Register(t, apitest.RegisterOpts{Verify: true})
		s.Do(t, apitest.Req{Method: http.MethodGet, Path: "/rules", Token: stranger.Token, Household: h.ID}).Status(t, http.StatusNotFound)
	})
}

func TestDescriptions(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 14, 15, 0, 0, 0, time.UTC)
	s := apitest.New(t, apitest.WithNow(func() time.Time { return now }))
	u := s.Register(t, apitest.RegisterOpts{Verify: true})
	space := s.PersonalSpace(t, u)
	cash := s.Cash(t, u)
	for _, e := range []struct{ desc, date string }{
		{"Kiosco", "2026-11-10"}, {"Súper", "2026-11-01"}, {"Súper", "2026-10-01"}, {"super", "2026-11-12"},
		{"Kiosco", "2026-09-01"}, {"Viaje viejo", "2025-11-13"}, {"Viaje", "2025-11-14"},
	} {
		s.CreateExpense(t, u, space, map[string]any{"amount": 100, "paymentMethodId": cash.ID, "description": e.desc, "spentAt": e.date})
	}
	var got []struct {
		Text string `json:"text"`
		Uses int    `json:"uses"`
	}
	s.Do(t, apitest.Req{Method: http.MethodGet, Path: "/descriptions", Token: u.Token, Household: space}).
		Status(t, http.StatusOK).JSON(t, &got)
	if len(got) != 3 || got[0].Text != "Súper" || got[0].Uses != 3 || got[1].Text != "Kiosco" || got[1].Uses != 2 ||
		got[2].Text != "Viaje" {
		t.Errorf("descriptions %+v", got)
	}
}
