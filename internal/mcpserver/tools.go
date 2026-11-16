package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Read-only tools (plan 8.6a). Input fields are documented through their
// jsonschema tags, which the assistant reads.

var readOnly = &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)}

func ptr[T any](v T) *T { return &v }

func (s *Server) addTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "list_spaces", Annotations: readOnly,
		Description: `The person's spaces: "personal" (Mi plata) and "hogar" (shared household) when they have one, with each base currency.`},
		run(s, s.listSpaces))
	mcp.AddTool(srv, &mcp.Tool{Name: "get_month_summary", Annotations: readOnly,
		Description: "Totals of one month in a space: spent, earned, by category, by payment method, compared with the previous month."},
		run(s, s.monthSummary))
	mcp.AddTool(srv, &mcp.Tool{Name: "list_expenses", Annotations: readOnly,
		Description: "Confirmed expenses of a space between two dates, newest first, with totals of the whole filtered list. Filter by category name or by words in the description."},
		run(s, s.listExpenses))
	mcp.AddTool(srv, &mcp.Tool{Name: "list_payment_methods", Annotations: readOnly,
		Description: "The person's own active payment methods (cash, debit, credit cards, wallets). Names are used by get_card_statements and simulate_purchase."},
		run(s, s.listPaymentMethods))
	mcp.AddTool(srv, &mcp.Tool{Name: "get_committed_payments", Annotations: readOnly,
		Description: `What the person's credit cards already owe in the coming months ("Comprometido"): per card and in total, in ARS and USD.`},
		run(s, s.committed))
	mcp.AddTool(srv, &mcp.Tool{Name: "get_card_statements", Annotations: readOnly,
		Description: "Monthly statements of one of the person's credit cards: closing and due dates, totals, minimum, paid or not."},
		run(s, s.cardStatements))
	mcp.AddTool(srv, &mcp.Tool{Name: "get_budgets_progress", Annotations: readOnly,
		Description: "Active budgets and savings goals of a space with how much is used this period."},
		run(s, s.budgets))
	mcp.AddTool(srv, &mcp.Tool{Name: "get_exchange_rates", Annotations: readOnly,
		Description: "Current dollar and euro rates in Argentina (oficial, MEP, blue, tarjeta, cripto) with when each was last confirmed."},
		run(s, s.rates))
	mcp.AddTool(srv, &mcp.Tool{Name: "simulate_purchase", Annotations: readOnly,
		Description: "What-if for a purchase, saving nothing: how the installments would fall on the person's months, with what is already committed, recurring expenses and incomes, and budgets it would affect."},
		run(s, s.simulate))
}

// ---------- spaces ----------

type spaceIn struct {
	Space string `json:"space,omitempty" jsonschema:"personal (default) or hogar"`
}

type household struct {
	ID           uuid.UUID `json:"id"`
	Name         string    `json:"name"`
	BaseCurrency string    `json:"baseCurrency"`
	Kind         string    `json:"kind"`
}

func (s *Server) households(ctx context.Context, c caller) ([]household, error) {
	var list []household
	err := s.call(ctx, c, http.MethodGet, "/households", nil, uuid.Nil, nil, &list)
	return list, err
}

// space resolves "personal" or "hogar" to the person's space.
func (s *Server) space(ctx context.Context, c caller, name string) (household, error) {
	kind := "personal"
	switch fold(name) {
	case "", "personal", "mi plata", "yo":
	case "hogar", "shared", "household":
		kind = "shared"
	default:
		return household{}, toolErr(`Unknown space %q: use "personal" or "hogar".`, name)
	}
	list, err := s.households(ctx, c)
	if err != nil {
		return household{}, err
	}
	for _, h := range list {
		if h.Kind == kind {
			return h, nil
		}
	}
	if kind == "shared" {
		return household{}, toolErr("The person has no hogar (shared household).")
	}
	return household{}, toolErr("The person has no personal space yet.")
}

func spaceName(kind string) string {
	if kind == "shared" {
		return "hogar"
	}
	return "personal"
}

type spaceOut struct {
	Space        string `json:"space"`
	Name         string `json:"name"`
	BaseCurrency string `json:"baseCurrency"`
}

func (s *Server) listSpaces(ctx context.Context, c caller, _ struct{}) (any, error) {
	list, err := s.households(ctx, c)
	if err != nil {
		return nil, err
	}
	out := make([]spaceOut, 0, len(list))
	for _, h := range list {
		out = append(out, spaceOut{Space: spaceName(h.Kind), Name: h.Name, BaseCurrency: h.BaseCurrency})
	}
	return map[string]any{"spaces": out}, nil
}

// ---------- month summary ----------

type monthIn struct {
	Space string `json:"space,omitempty" jsonschema:"personal (default) or hogar"`
	Month string `json:"month,omitempty" jsonschema:"YYYY-MM; default the current month"`
}

func (s *Server) monthSummary(ctx context.Context, c caller, in monthIn) (any, error) {
	h, err := s.space(ctx, c, in.Space)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if in.Month != "" {
		q.Set("month", in.Month)
	}
	var out json.RawMessage
	if err := s.call(ctx, c, http.MethodGet, "/reports/monthly", q, h.ID, nil, &out); err != nil {
		return nil, err
	}
	return map[string]any{"space": spaceName(h.Kind), "summary": out}, nil
}

// ---------- expenses ----------

type expensesIn struct {
	Space    string `json:"space,omitempty" jsonschema:"personal (default) or hogar"`
	From     string `json:"from" jsonschema:"first day, YYYY-MM-DD"`
	To       string `json:"to" jsonschema:"last day, YYYY-MM-DD"`
	Category string `json:"category,omitempty" jsonschema:"a category name of the space (e.g. Supermercado)"`
	Search   string `json:"search,omitempty" jsonschema:"words in the description"`
	Limit    int    `json:"limit,omitempty" jsonschema:"items per page, 1 to 100, default 50"`
	Offset   int    `json:"offset,omitempty" jsonschema:"items to skip, for the next page"`
}

type named struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type expenseRow struct {
	CategoryID      *string     `json:"categoryId"`
	PaymentMethodID string      `json:"paymentMethodId"`
	Amount          json.Number `json:"amount"`
	Currency        string      `json:"currency"`
	AmountBase      json.Number `json:"amountBase"`
	BaseCurrency    string      `json:"baseCurrency"`
	Description     string      `json:"description"`
	SpentAt         string      `json:"spentAt"`
	Installments    int         `json:"installments"`
	IsShared        bool        `json:"isShared"`
}

type expenseOut struct {
	Date          string      `json:"date"`
	Description   string      `json:"description"`
	Amount        json.Number `json:"amount"`
	Currency      string      `json:"currency"`
	AmountBase    json.Number `json:"amountBase"`
	Category      string      `json:"category,omitempty"`
	PaymentMethod string      `json:"paymentMethod,omitempty"`
	Installments  int         `json:"installments,omitempty"`
	Shared        bool        `json:"shared,omitempty"`
}

func validDate(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

func (s *Server) listExpenses(ctx context.Context, c caller, in expensesIn) (any, error) {
	if !validDate(in.From) || !validDate(in.To) {
		return nil, toolErr("from and to are required, as YYYY-MM-DD.")
	}
	h, err := s.space(ctx, c, in.Space)
	if err != nil {
		return nil, err
	}
	var cats []named
	if err := s.call(ctx, c, http.MethodGet, "/categories", nil, h.ID, nil, &cats); err != nil {
		return nil, err
	}
	catName := make(map[string]string, len(cats))
	q := url.Values{"from": {in.From}, "to": {in.To}}
	for _, ct := range cats {
		catName[ct.ID] = ct.Name
		if in.Category != "" && fold(ct.Name) == fold(in.Category) {
			q.Set("categoryId", ct.ID)
		}
	}
	if in.Category != "" && q.Get("categoryId") == "" {
		names := make([]string, len(cats))
		for i, ct := range cats {
			names[i] = ct.Name
		}
		return nil, toolErr("No category %q in this space. Categories: %s.", in.Category, strings.Join(names, ", "))
	}
	if search := strings.TrimSpace(in.Search); search != "" {
		q.Set("q", search)
	}
	limit := clamp(in.Limit, 1, 100, 50)
	q.Set("limit", itoa(limit))
	q.Set("offset", itoa(max(0, in.Offset)))

	var page struct {
		Items           []expenseRow `json:"items"`
		TotalCount      int64        `json:"totalCount"`
		TotalAmountBase json.Number  `json:"totalAmountBase"`
	}
	if err := s.call(ctx, c, http.MethodGet, "/expenses", q, h.ID, nil, &page); err != nil {
		return nil, err
	}
	var methods []named
	if err := s.call(ctx, c, http.MethodGet, "/payment-methods", url.Values{"includeInactive": {"true"}}, uuid.Nil, nil, &methods); err != nil {
		return nil, err
	}
	pmName := make(map[string]string, len(methods))
	for _, m := range methods {
		pmName[m.ID] = m.Name
	}
	items := make([]expenseOut, len(page.Items))
	for i, e := range page.Items {
		o := expenseOut{
			Date: e.SpentAt, Description: e.Description, Amount: e.Amount, Currency: e.Currency,
			AmountBase: e.AmountBase, PaymentMethod: pmName[e.PaymentMethodID], Shared: e.IsShared,
		}
		if e.CategoryID != nil {
			o.Category = catName[*e.CategoryID]
		}
		if e.Installments > 1 {
			o.Installments = e.Installments
		}
		items[i] = o
	}
	return map[string]any{
		"space": spaceName(h.Kind), "baseCurrency": h.BaseCurrency,
		"totalCount": page.TotalCount, "totalAmountBase": page.TotalAmountBase,
		"offset": max(0, in.Offset), "items": items,
	}, nil
}

// ---------- payment methods and cards ----------

type paymentMethod struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Kind               string `json:"kind"`
	AllowsInstallments bool   `json:"allowsInstallments"`
}

func (s *Server) paymentMethods(ctx context.Context, c caller) ([]paymentMethod, error) {
	var list []paymentMethod
	err := s.call(ctx, c, http.MethodGet, "/payment-methods", nil, uuid.Nil, nil, &list)
	return list, err
}

func (s *Server) listPaymentMethods(ctx context.Context, c caller, _ struct{}) (any, error) {
	list, err := s.paymentMethods(ctx, c)
	if err != nil {
		return nil, err
	}
	type pmOut struct {
		Name               string `json:"name"`
		Kind               string `json:"kind"`
		AllowsInstallments bool   `json:"allowsInstallments"`
	}
	out := make([]pmOut, len(list))
	for i, p := range list {
		out[i] = pmOut{Name: p.Name, Kind: p.Kind, AllowsInstallments: p.AllowsInstallments}
	}
	return map[string]any{"paymentMethods": out}, nil
}

// paymentMethod finds one of the person's methods by name; credit limits
// the search to credit cards.
func (s *Server) paymentMethod(ctx context.Context, c caller, name string, credit bool) (paymentMethod, error) {
	list, err := s.paymentMethods(ctx, c)
	if err != nil {
		return paymentMethod{}, err
	}
	var names []string
	for _, p := range list {
		if credit && p.Kind != "credit" {
			continue
		}
		if fold(p.Name) == fold(name) {
			return p, nil
		}
		names = append(names, p.Name)
	}
	what := "payment method"
	if credit {
		what = "credit card"
	}
	if len(names) == 0 {
		return paymentMethod{}, toolErr("The person has no %s.", what)
	}
	return paymentMethod{}, toolErr("No %s called %q. The person has: %s.", what, name, strings.Join(names, ", "))
}

type committedIn struct {
	Months int `json:"months,omitempty" jsonschema:"how many months ahead, 1 to 12, default 6"`
}

func (s *Server) committed(ctx context.Context, c caller, in committedIn) (any, error) {
	var out json.RawMessage
	q := url.Values{"months": {itoa(clamp(in.Months, 1, 12, 6))}}
	if err := s.call(ctx, c, http.MethodGet, "/credit-cards/committed", q, uuid.Nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

type statementsIn struct {
	Card string `json:"card" jsonschema:"the credit card's name, as list_payment_methods shows it"`
	From string `json:"from,omitempty" jsonschema:"first statement month, YYYY-MM"`
	To   string `json:"to,omitempty" jsonschema:"last statement month, YYYY-MM"`
}

func (s *Server) cardStatements(ctx context.Context, c caller, in statementsIn) (any, error) {
	if strings.TrimSpace(in.Card) == "" {
		return nil, toolErr("card is required: the credit card's name.")
	}
	pm, err := s.paymentMethod(ctx, c, in.Card, true)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if in.From != "" {
		q.Set("from", in.From)
	}
	if in.To != "" {
		q.Set("to", in.To)
	}
	var out json.RawMessage
	if err := s.call(ctx, c, http.MethodGet, "/payment-methods/"+url.PathEscape(pm.ID)+"/credit-card/statements", q, uuid.Nil, nil, &out); err != nil {
		return nil, err
	}
	return map[string]any{"card": pm.Name, "statements": out}, nil
}

// ---------- budgets, rates, simulation ----------

func (s *Server) budgets(ctx context.Context, c caller, in spaceIn) (any, error) {
	h, err := s.space(ctx, c, in.Space)
	if err != nil {
		return nil, err
	}
	var out json.RawMessage
	if err := s.call(ctx, c, http.MethodGet, "/goals/progress", url.Values{"active": {"true"}}, h.ID, nil, &out); err != nil {
		return nil, err
	}
	return map[string]any{"space": spaceName(h.Kind), "budgets": out}, nil
}

func (s *Server) rates(ctx context.Context, c caller, _ struct{}) (any, error) {
	var out json.RawMessage
	if err := s.call(ctx, c, http.MethodGet, "/exchange-rates/current", nil, uuid.Nil, nil, &out); err != nil {
		return nil, err
	}
	return map[string]any{"rates": out}, nil
}

type simulateIn struct {
	Space         string  `json:"space,omitempty" jsonschema:"personal (default) or hogar"`
	Amount        float64 `json:"amount" jsonschema:"total price"`
	Currency      string  `json:"currency,omitempty" jsonschema:"ARS (default), USD or EUR"`
	Installments  int     `json:"installments,omitempty" jsonschema:"number of installments, default 1"`
	PaymentMethod string  `json:"payment_method" jsonschema:"the payment method's name, as list_payment_methods shows it"`
	StartDate     string  `json:"start_date,omitempty" jsonschema:"purchase date, YYYY-MM-DD; default today"`
	Months        int     `json:"months,omitempty" jsonschema:"months to project, 1 to 12, default 6"`
}

func (s *Server) simulate(ctx context.Context, c caller, in simulateIn) (any, error) {
	if in.Amount <= 0 {
		return nil, toolErr("amount must be greater than zero.")
	}
	if strings.TrimSpace(in.PaymentMethod) == "" {
		return nil, toolErr("payment_method is required: the name of one of the person's payment methods.")
	}
	h, err := s.space(ctx, c, in.Space)
	if err != nil {
		return nil, err
	}
	pm, err := s.paymentMethod(ctx, c, in.PaymentMethod, false)
	if err != nil {
		return nil, err
	}
	currency := strings.ToUpper(strings.TrimSpace(in.Currency))
	if currency == "" {
		currency = "ARS"
	}
	body := map[string]any{
		"paymentMethodId": pm.ID, "amount": in.Amount, "currency": currency,
		"installments": clamp(in.Installments, 1, 99, 1), "months": clamp(in.Months, 1, 12, 6),
	}
	if in.StartDate != "" {
		body["spentAt"] = in.StartDate
	}
	var out json.RawMessage
	if err := s.call(ctx, c, http.MethodPost, "/simulations/purchase", nil, h.ID, body, &out); err != nil {
		return nil, err
	}
	return map[string]any{"space": spaceName(h.Kind), "paymentMethod": pm.Name, "simulation": out}, nil
}
