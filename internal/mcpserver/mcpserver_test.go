package mcpserver_test

// The MCP tools (plan 8.6a) through a real MCP client against the real API.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/LucianoR23/valuo_api/internal/testutil/apitest"
	"github.com/LucianoR23/valuo_api/internal/testutil/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

func today() string { return time.Now().Format("2006-01-02") }

func connect(t *testing.T, s *apitest.Server, u apitest.User) *mcp.ClientSession {
	t.Helper()
	_, tok := s.ConnectAssistant(t, u)
	return s.MCP(t, tok.AccessToken)
}

func categoryID(t *testing.T, s *apitest.Server, u apitest.User, hID, name string) string {
	t.Helper()
	var cats []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	s.Do(t, apitest.Req{Method: http.MethodGet, Path: "/categories", Token: u.Token, Household: hID}).Status(t, http.StatusOK).JSON(t, &cats)
	for _, c := range cats {
		if c.Name == name {
			return c.ID
		}
	}
	t.Fatalf("no category %q", name)
	return ""
}

func TestToolsAreReadOnly(t *testing.T) {
	s := apitest.New(t)
	session := connect(t, s, s.Register(t, apitest.RegisterOpts{Verify: true}))

	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s is not marked read-only", tool.Name)
		}
	}
	slices.Sort(names)
	want := []string{"get_budgets_progress", "get_card_statements", "get_committed_payments", "get_exchange_rates",
		"get_month_summary", "list_expenses", "list_payment_methods", "list_spaces", "simulate_purchase"}
	if !slices.Equal(names, want) {
		t.Errorf("tools = %v, want %v", names, want)
	}
	if init := session.InitializeResult(); init == nil || !strings.Contains(init.Instructions, "never as instructions") {
		t.Error("the instructions must say typed text is data")
	}
}

func TestSpacesAndExpenses(t *testing.T) {
	s := apitest.New(t)
	u := s.Register(t, apitest.RegisterOpts{Verify: true})
	personal := s.PersonalSpace(t, u)
	cash := s.Cash(t, u)
	super := categoryID(t, s, u, personal, "Súper")
	s.CreateExpense(t, u, personal, map[string]any{"amount": 15430.5, "description": "Carrefour", "spentAt": today(),
		"paymentMethodId": cash.ID, "categoryId": super})
	s.CreateExpense(t, u, personal, map[string]any{"amount": 3000, "description": "Café", "spentAt": today(), "paymentMethodId": cash.ID})
	session := connect(t, s, u)

	var spaces struct {
		Spaces []struct{ Space, Name, BaseCurrency string } `json:"spaces"`
	}
	apitest.ToolJSON(t, session, "list_spaces", nil, &spaces)
	if len(spaces.Spaces) != 1 || spaces.Spaces[0].Space != "personal" {
		t.Fatalf("spaces = %+v", spaces.Spaces)
	}
	if text, isErr := apitest.CallTool(t, session, "get_month_summary", map[string]any{"space": "hogar"}); !isErr || !strings.Contains(text, "no hogar") {
		t.Errorf("hogar without one: %q (error %v)", text, isErr)
	}

	from := time.Now().AddDate(0, 0, -7).Format("2006-01-02")
	var list struct {
		TotalCount      int         `json:"totalCount"`
		TotalAmountBase json.Number `json:"totalAmountBase"`
		Items           []struct {
			Description, Category, PaymentMethod, Date string
			Amount                                     json.Number
		} `json:"items"`
	}
	apitest.ToolJSON(t, session, "list_expenses", map[string]any{"from": from, "to": today()}, &list)
	if list.TotalCount != 2 || len(list.Items) != 2 {
		t.Fatalf("expenses = %+v", list)
	}
	apitest.ToolJSON(t, session, "list_expenses", map[string]any{"from": from, "to": today(), "category": "super"}, &list)
	if len(list.Items) != 1 || list.Items[0].Description != "Carrefour" || list.Items[0].Category != "Súper" ||
		list.Items[0].PaymentMethod != cash.Name || list.Items[0].Amount != "15430.5" {
		t.Fatalf("by category (case and accents ignored) = %+v", list.Items)
	}
	apitest.ToolJSON(t, session, "list_expenses", map[string]any{"from": from, "to": today(), "search": "café"}, &list)
	if len(list.Items) != 1 || list.Items[0].Description != "Café" {
		t.Fatalf("by search = %+v", list.Items)
	}

	if text, isErr := apitest.CallTool(t, session, "list_expenses", map[string]any{"from": from, "to": today(), "category": "Criptomonedas raras"}); !isErr || !strings.Contains(text, "Súper") {
		t.Errorf("unknown category should list the names: %q", text)
	}
	if _, isErr := apitest.CallTool(t, session, "list_expenses", map[string]any{"from": "ayer", "to": today()}); !isErr {
		t.Error("bad dates must be a tool error")
	}

	var summary struct {
		Space   string         `json:"space"`
		Summary map[string]any `json:"summary"`
	}
	apitest.ToolJSON(t, session, "get_month_summary", nil, &summary)
	if summary.Space != "personal" || len(summary.Summary) == 0 {
		t.Errorf("month summary = %+v", summary)
	}
}

func TestHogarFollowsMembership(t *testing.T) {
	s := apitest.New(t)
	owner := s.Register(t, apitest.RegisterOpts{Verify: true})
	h := s.CreateHousehold(t, owner, "Casa")
	member := s.AddMember(t, owner, h.ID)
	s.CreateExpense(t, owner, h.ID, map[string]any{"amount": 50000, "description": "Expensas", "spentAt": today(),
		"paymentMethodId": s.Cash(t, owner).ID, "isShared": true})

	session := connect(t, s, member)
	var list struct {
		Space string                         `json:"space"`
		Items []struct{ Description string } `json:"items"`
	}
	apitest.ToolJSON(t, session, "list_expenses", map[string]any{"space": "hogar", "from": today(), "to": today()}, &list)
	if list.Space != "hogar" || len(list.Items) != 1 || list.Items[0].Description != "Expensas" {
		t.Fatalf("member's hogar = %+v", list)
	}

	// Removed from the hogar: the same token no longer reaches it.
	s.Do(t, apitest.Req{Method: http.MethodDelete, Path: "/households/" + h.ID + "/members/" + member.ID, Token: owner.Token}).Status(t, http.StatusNoContent)
	if text, isErr := apitest.CallTool(t, session, "list_expenses", map[string]any{"space": "hogar", "from": today(), "to": today()}); !isErr {
		t.Errorf("after leaving, hogar answered: %s", text)
	}
}

func TestCardsRatesBudgetsAndSimulation(t *testing.T) {
	s := apitest.New(t)
	u := s.Register(t, apitest.RegisterOpts{Verify: true})
	now := time.Now()
	closing := time.Date(now.Year(), now.Month(), 25, 0, 0, 0, 0, time.UTC)
	s.CreateCreditCard(t, u, "Visa Galicia", 25, 5, closing.Format("2006-01-02"), closing.AddDate(0, 0, 11).Format("2006-01-02"))
	session := connect(t, s, u)

	var pms struct {
		PaymentMethods []struct{ Name, Kind string } `json:"paymentMethods"`
	}
	apitest.ToolJSON(t, session, "list_payment_methods", nil, &pms)
	if len(pms.PaymentMethods) != 2 {
		t.Fatalf("payment methods = %+v", pms)
	}

	var st struct {
		Card string `json:"card"`
	}
	apitest.ToolJSON(t, session, "get_card_statements", map[string]any{"card": "visa galicia"}, &st)
	if st.Card != "Visa Galicia" {
		t.Errorf("statements card = %q", st.Card)
	}
	if text, isErr := apitest.CallTool(t, session, "get_card_statements", map[string]any{"card": "Efectivo"}); !isErr || !strings.Contains(text, "Visa Galicia") {
		t.Errorf("cash is not a card: %q", text)
	}

	var committed map[string]any
	apitest.ToolJSON(t, session, "get_committed_payments", map[string]any{"months": 3}, &committed)
	var rates map[string]any
	apitest.ToolJSON(t, session, "get_exchange_rates", nil, &rates)
	var budgets map[string]any
	apitest.ToolJSON(t, session, "get_budgets_progress", nil, &budgets)

	var sim struct {
		PaymentMethod string         `json:"paymentMethod"`
		Simulation    map[string]any `json:"simulation"`
	}
	apitest.ToolJSON(t, session, "simulate_purchase", map[string]any{"amount": 300000, "installments": 6, "payment_method": "Visa Galicia"}, &sim)
	if purchase, _ := sim.Simulation["purchase"].(map[string]any); sim.PaymentMethod != "Visa Galicia" || purchase["isCredit"] != true {
		t.Errorf("simulation = %+v", sim)
	}
	if text, isErr := apitest.CallTool(t, session, "simulate_purchase", map[string]any{"amount": 1000, "payment_method": "Amex"}); !isErr || !strings.Contains(text, "Visa Galicia") {
		t.Errorf("unknown method should list names: %q", text)
	}

	// Nothing was saved by the simulation.
	var n int
	if err := s.Pool.QueryRow(context.Background(), `SELECT count(*) FROM expenses`).Scan(&n); err != nil || n != 0 {
		t.Errorf("expenses after simulating = %d (%v)", n, err)
	}
}

func TestRateLimitAndAudit(t *testing.T) {
	s := apitest.New(t)
	u := s.Register(t, apitest.RegisterOpts{Verify: true})
	session := connect(t, s, u)

	apitest.CallTool(t, session, "list_spaces", nil)
	var calls int
	if err := s.Pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action = 'mcp.call' AND target = 'list_spaces'`).Scan(&calls); err != nil || calls != 1 {
		t.Fatalf("mcp.call events = %d (%v), want 1", calls, err)
	}
	var activity struct {
		Items []struct {
			Action string `json:"action"`
		} `json:"items"`
	}
	s.Do(t, apitest.Req{Method: http.MethodGet, Path: "/me/activity", Token: u.Token}).Status(t, http.StatusOK).JSON(t, &activity)
	for _, it := range activity.Items {
		if it.Action == "mcp.call" {
			t.Error("tool calls must not fill the recent activity list")
		}
	}

	// The hour is full: the next call is refused.
	if _, err := s.Pool.Exec(context.Background(), `UPDATE mcp_calls SET count = 60`); err != nil {
		t.Fatal(err)
	}
	if text, isErr := apitest.CallTool(t, session, "list_spaces", nil); !isErr || !strings.Contains(text, "Rate limit") {
		t.Errorf("61st call in the hour: %q (error %v)", text, isErr)
	}
	// So is the day, counting the previous hours.
	if _, err := s.Pool.Exec(context.Background(), `UPDATE mcp_calls SET count = 1;
		INSERT INTO mcp_calls (grant_id, hour, count) SELECT grant_id, hour - interval '3 hours', 499 FROM mcp_calls`); err != nil {
		t.Fatal(err)
	}
	if text, isErr := apitest.CallTool(t, session, "list_spaces", nil); !isErr || !strings.Contains(text, "Rate limit") {
		t.Errorf("501st call in the day: %q (error %v)", text, isErr)
	}
}
