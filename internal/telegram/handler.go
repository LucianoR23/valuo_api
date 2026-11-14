package telegram

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/LucianoR23/valuo_api/internal/auth"
	"github.com/LucianoR23/valuo_api/internal/httpx"
)

// SecretHeader carries the secret given to setWebhook.
const SecretHeader = "X-Telegram-Bot-Api-Secret-Token" //nolint:gosec // G101: a header name, not a credential

// maxUpdateBytes caps one update; text messages are a few KB.
const maxUpdateBytes = 256 << 10

type Handler struct {
	svc    *Service // nil when the bot is off
	secret string
	authMW *auth.Middleware
	logger *slog.Logger
}

func NewHandler(svc *Service, secret string, authMW *auth.Middleware, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, secret: secret, authMW: authMW, logger: logger}
}

// Mount: Telegram's webhook (secret header, no session) and the app's
// routes to link and unlink. With the bot off every route answers 503.
func (h *Handler) Mount(r chi.Router) {
	r.Post("/telegram/webhook", h.Webhook)
	r.Group(func(r chi.Router) {
		r.Use(h.authMW.RequireAuth)
		r.Get("/telegram/link", h.Status)
		r.Delete("/telegram/link", h.Unlink)
		r.With(h.authMW.RequireVerifiedEmail).Post("/telegram/link", h.NewCode)
	})
}

func (h *Handler) off(w http.ResponseWriter) bool {
	if h.svc == nil {
		httpx.WriteJSON(w, http.StatusServiceUnavailable, httpx.ErrorResponse{Code: "telegram_off", Message: "el bot de Telegram no está disponible"})
		return true
	}
	return false
}

// Webhook: 200 for every update with the right secret, also when it could
// not be handled, so Telegram does not resend it.
func (h *Handler) Webhook(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	got := r.Header.Get(SecretHeader)
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(h.secret)) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var u Update
	if err := json.NewDecoder(io.LimitReader(r.Body, maxUpdateBytes)).Decode(&u); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	h.svc.Handle(r.Context(), u)
	w.WriteHeader(http.StatusOK)
}

type statusDTO struct {
	Linked   bool       `json:"linked"`
	LinkedAt *time.Time `json:"linkedAt"`
}

func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.user(w, r)
	if !ok {
		return
	}
	at, err := h.svc.LinkedAt(r.Context(), userID)
	if err != nil {
		httpx.WriteError(w, r, h.logger, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, statusDTO{Linked: at != nil, LinkedAt: at})
}

type codeDTO struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func (h *Handler) NewCode(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.user(w, r)
	if !ok {
		return
	}
	url, exp, err := h.svc.NewLinkCode(r.Context(), userID)
	if err != nil {
		httpx.WriteError(w, r, h.logger, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, codeDTO{URL: url, ExpiresAt: exp.UTC()})
}

func (h *Handler) Unlink(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.user(w, r)
	if !ok {
		return
	}
	if err := h.svc.Unlink(r.Context(), userID); err != nil {
		httpx.WriteError(w, r, h.logger, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) user(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	if h.off(w) {
		return uuid.Nil, false
	}
	userID, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, h.logger, errors.New("middleware faltante"))
		return uuid.Nil, false
	}
	return userID, true
}
