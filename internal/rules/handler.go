package rules

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/LucianoR23/valuo_api/internal/auth"
	"github.com/LucianoR23/valuo_api/internal/domain"
	"github.com/LucianoR23/valuo_api/internal/households"
	"github.com/LucianoR23/valuo_api/internal/httpx"
)

// Handler serves /rules and /descriptions of the space in X-Household-ID.
type Handler struct {
	svc    *Service
	logger *slog.Logger
	authMW *auth.Middleware
	hhMW   *households.Middleware
}

func NewHandler(svc *Service, authMW *auth.Middleware, hhMW *households.Middleware, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, logger: logger, authMW: authMW, hhMW: hhMW}
}

// Mount registers the routes under auth + household member.
//   - GET    /rules        the space's rules
//   - POST   /rules        new rule
//   - PATCH  /rules/{id}   replace a rule
//   - DELETE /rules/{id}
//   - GET    /descriptions  recent descriptions for autocomplete
func (h *Handler) Mount(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(h.authMW.RequireAuth)
		r.Use(h.hhMW.RequireHouseholdMember)

		r.Get("/descriptions", h.Descriptions)
		r.Route("/rules", func(r chi.Router) {
			r.Get("/", h.List)
			r.Post("/", h.Create)
			r.Patch("/{id}", h.Update)
			r.Delete("/{id}", h.Delete)
		})
	})
}

type ruleDTO struct {
	ID                 string           `json:"id"`
	HouseholdID        string           `json:"householdId"`
	CreatedBy          string           `json:"createdBy"`
	MatchText          string           `json:"matchText"`
	PaymentMethodID    *uuid.UUID       `json:"paymentMethodId"`
	MinAmount          *decimal.Decimal `json:"minAmount"`
	MaxAmount          *decimal.Decimal `json:"maxAmount"`
	SetCategoryID      *uuid.UUID       `json:"setCategoryId"`
	SetPaymentMethodID *uuid.UUID       `json:"setPaymentMethodId"`
	CreatedAt          time.Time        `json:"createdAt"`
	UpdatedAt          time.Time        `json:"updatedAt"`
}

type ruleRequest struct {
	MatchText          string           `json:"matchText"`
	PaymentMethodID    *uuid.UUID       `json:"paymentMethodId"`
	MinAmount          *decimal.Decimal `json:"minAmount"`
	MaxAmount          *decimal.Decimal `json:"maxAmount"`
	SetCategoryID      *uuid.UUID       `json:"setCategoryId"`
	SetPaymentMethodID *uuid.UUID       `json:"setPaymentMethodId"`
}

func (q ruleRequest) input() Input {
	return Input{
		MatchText: q.MatchText, PaymentMethodID: q.PaymentMethodID,
		MinAmount: q.MinAmount, MaxAmount: q.MaxAmount,
		SetCategoryID: q.SetCategoryID, SetPaymentMethodID: q.SetPaymentMethodID,
	}
}

type descriptionDTO struct {
	Text string `json:"text"`
	Uses int    `json:"uses"`
}

func toDTO(r Rule) ruleDTO {
	return ruleDTO{
		ID: r.ID.String(), HouseholdID: r.HouseholdID.String(), CreatedBy: r.CreatedBy.String(),
		MatchText: r.MatchText, PaymentMethodID: r.PaymentMethodID,
		MinAmount: r.MinAmount, MaxAmount: r.MaxAmount,
		SetCategoryID: r.SetCategoryID, SetPaymentMethodID: r.SetPaymentMethodID,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	householdID, ok := households.HouseholdIDFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, h.logger, errors.New("middleware faltante"))
		return
	}
	list, err := h.svc.List(r.Context(), householdID)
	if err != nil {
		httpx.WriteError(w, r, h.logger, err)
		return
	}
	out := make([]ruleDTO, len(list))
	for i, rule := range list {
		out[i] = toDTO(rule)
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	householdID, ok := households.HouseholdIDFrom(r.Context())
	userID, okUser := auth.UserIDFrom(r.Context())
	if !ok || !okUser {
		httpx.WriteError(w, r, h.logger, errors.New("middleware faltante"))
		return
	}
	var req ruleRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.WriteError(w, r, h.logger, err)
		return
	}
	rule, err := h.svc.Create(r.Context(), userID, householdID, req.input())
	if err != nil {
		httpx.WriteError(w, r, h.logger, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toDTO(rule))
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	householdID, ok := households.HouseholdIDFrom(r.Context())
	userID, okUser := auth.UserIDFrom(r.Context())
	if !ok || !okUser {
		httpx.WriteError(w, r, h.logger, errors.New("middleware faltante"))
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, h.logger, domain.ErrNotFound)
		return
	}
	var req ruleRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.WriteError(w, r, h.logger, err)
		return
	}
	rule, err := h.svc.Update(r.Context(), userID, householdID, id, req.input())
	if err != nil {
		httpx.WriteError(w, r, h.logger, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toDTO(rule))
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	householdID, ok := households.HouseholdIDFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, h.logger, errors.New("middleware faltante"))
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, h.logger, domain.ErrNotFound)
		return
	}
	if err := h.svc.Delete(r.Context(), householdID, id); err != nil {
		httpx.WriteError(w, r, h.logger, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) Descriptions(w http.ResponseWriter, r *http.Request) {
	householdID, ok := households.HouseholdIDFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, h.logger, errors.New("middleware faltante"))
		return
	}
	list, err := h.svc.Descriptions(r.Context(), householdID)
	if err != nil {
		httpx.WriteError(w, r, h.logger, err)
		return
	}
	out := make([]descriptionDTO, len(list))
	for i, d := range list {
		out[i] = descriptionDTO{Text: d.Text, Uses: d.Uses}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
