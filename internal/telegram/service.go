// Package telegram is the Telegram bot (plan 3.6): a person links their chat
// from the app, then sends expenses as text; each one is shown back with
// ✅ ✏️ ❌ buttons and saved only on ✅. Unlinked chats get a fixed reply,
// with no AI call (plan §8.1).
package telegram

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/shopspring/decimal"

	sqlcgen "github.com/LucianoR23/valuo_api/internal/db/sqlc"
	"github.com/LucianoR23/valuo_api/internal/domain"
	"github.com/LucianoR23/valuo_api/internal/expenseparse"
	"github.com/LucianoR23/valuo_api/internal/expenses"
)

const (
	// CodeTTL is how long a link from the app works (plan 3.6).
	CodeTTL = 10 * time.Minute
	// DraftRetention: drafts and saved rows (for /deshacer) are kept a day.
	DraftRetention = 24 * time.Hour
	// UndoWindow is how far back /deshacer reaches.
	UndoWindow = 24 * time.Hour
	// EditWindow: after ✏️, a message within this time corrects the draft.
	EditWindow = time.Hour
	// MaxMethodButtons is how many payment methods are offered (decided with
	// the user: the 6 most used).
	MaxMethodButtons = 6
	// CommittedMonths is how many months /comprometido lists.
	CommittedMonths = 3
	// usage window for "most used" methods.
	methodUsageWindow = 90 * 24 * time.Hour
)

// Bot is the part of the Bot API client the service uses.
type Bot interface {
	SendMessage(ctx context.Context, chatID int64, text string, kb Keyboard) (int64, error)
	EditMessage(ctx context.Context, chatID, messageID int64, text string, kb Keyboard) error
	AnswerCallback(ctx context.Context, id, text string) error
	GetFile(ctx context.Context, fileID string) (string, error)
	Download(ctx context.Context, path string, max int64) ([]byte, error)
}

// Parser reads a sentence, a voice note or an image (expenseparse.Parser via
// the Telegram channel).
type Parser interface {
	Parse(ctx context.Context, userID, householdID uuid.UUID, text string) (expenseparse.Result, error)
	ParseAudio(ctx context.Context, userID, householdID uuid.UUID, mimeType string, data []byte) (expenseparse.Result, error)
	ParseReceipt(ctx context.Context, userID, householdID uuid.UUID, mimeType string, data []byte) (expenseparse.Result, error)
}

// Spaces finds the person's "Mi plata" and Hogar.
type Spaces interface {
	PersonalForUser(ctx context.Context, userID uuid.UUID) (domain.Household, error)
	ActiveSharedForUser(ctx context.Context, userID uuid.UUID) (domain.Household, error)
}

// Expenses saves and undoes expenses (expenses.Service).
type Expenses interface {
	Create(ctx context.Context, in expenses.CreateInput) (domain.ExpenseDetail, error)
	Delete(ctx context.Context, householdID, id, actorUserID uuid.UUID) error
}

// Committer is "Comprometido" (statements.Service).
type Committer interface {
	Committed(ctx context.Context, ownerID uuid.UUID, months int) (domain.Committed, error)
}

// Deps are the services the bot reuses.
type Deps struct {
	Queries     *sqlcgen.Queries
	Bot         Bot
	Parser      Parser
	Categorizer expenseparse.Categorizer // re-suggests a category for the Hogar
	Spaces      Spaces
	Expenses    Expenses
	Committer   Committer
	// CategoryName shows the category in the confirmation.
	CategoryName func(ctx context.Context, id uuid.UUID) (string, error)
}

// Config is the bot's settings.
type Config struct {
	// Username is the bot's @name without the @, for t.me links.
	Username string
	// Brand is the product name, for the reply to unlinked chats.
	Brand    string
	Location *time.Location
	Now      func() time.Time
	Logger   *slog.Logger
}

type Service struct {
	Deps
	cfg     Config
	limiter *chatLimiter
	seen    *recentIDs
}

func NewService(d Deps, cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Location == nil {
		cfg.Location = time.UTC
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Service{Deps: d, cfg: cfg, limiter: newChatLimiter(cfg.Now), seen: newRecentIDs(2048)}
}

func (s *Service) now() time.Time { return s.cfg.Now() }

// today is the current day in the app's time zone, as midnight UTC (how
// expenses store spent_at).
func (s *Service) today() time.Time {
	y, m, d := s.now().In(s.cfg.Location).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// ===================== linking (app side) =====================

var codeEncoding = base64.RawURLEncoding

func hashCode(code string) []byte {
	h := sha256.Sum256([]byte(code))
	return h[:]
}

// NewLinkCode replaces the person's code with a new one and returns the
// t.me link that sends it to the bot as /start <code>.
func (s *Service) NewLinkCode(ctx context.Context, userID uuid.UUID) (string, time.Time, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	code := codeEncoding.EncodeToString(b) // 32 characters of [A-Za-z0-9_-]
	expires := s.now().Add(CodeTTL)
	if err := s.Queries.DeleteTelegramLinkCodes(ctx, userID); err != nil {
		return "", time.Time{}, err
	}
	if err := s.Queries.CreateTelegramLinkCode(ctx, sqlcgen.CreateTelegramLinkCodeParams{
		CodeHash: hashCode(code), UserID: userID, ExpiresAt: ts(expires),
	}); err != nil {
		return "", time.Time{}, err
	}
	return "https://t.me/" + s.cfg.Username + "?start=" + code, expires, nil
}

// LinkedAt says whether the person has a chat linked, and since when.
func (s *Service) LinkedAt(ctx context.Context, userID uuid.UUID) (*time.Time, error) {
	l, err := s.Queries.GetTelegramLink(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t := l.LinkedAt.Time
	return &t, nil
}

// Unlink removes the link and tells the chat, best effort.
func (s *Service) Unlink(ctx context.Context, userID uuid.UUID) error {
	chatID, err := s.Queries.DeleteTelegramLink(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	s.send(ctx, chatID, textUnlinked, nil)
	return nil
}

// Cleanup deletes expired codes and drafts older than a day (daily job).
func (s *Service) Cleanup(ctx context.Context) (int64, error) {
	now := s.now()
	return s.Queries.DeleteOldTelegramRows(ctx, sqlcgen.DeleteOldTelegramRowsParams{
		Now: ts(now), DraftsBefore: ts(now.Add(-DraftRetention)),
	})
}

// ===================== updates =====================

// Handle processes one update. Errors are logged, never returned to
// Telegram: a failed update is not retried.
func (s *Service) Handle(ctx context.Context, u Update) {
	if !s.seen.add(u.UpdateID) {
		return // Telegram resent it
	}
	switch {
	case u.Message != nil:
		s.message(ctx, u.Message)
	case u.CallbackQuery != nil:
		s.callback(ctx, u.CallbackQuery)
	}
}

func (s *Service) user(ctx context.Context, chatID int64) (uuid.UUID, bool) {
	id, err := s.Queries.TelegramUserByChat(ctx, chatID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.cfg.Logger.ErrorContext(ctx, "telegram: link lookup failed", "error", err)
		}
		return uuid.Nil, false
	}
	return id, true
}

func (s *Service) message(ctx context.Context, m *Message) {
	// Only private chats: in a group the bot would read other people.
	if m.Chat.Type != "private" || m.From == nil || m.From.ID != m.Chat.ID {
		return
	}
	chatID := m.Chat.ID
	text := strings.TrimSpace(m.Text)
	if code, ok := startCode(text); ok {
		if !s.limiter.allow(chatID, false) {
			return
		}
		s.link(ctx, chatID, code)
		return
	}

	userID, linked := s.user(ctx, chatID)
	if !linked {
		// The fixed reply, at most once every few minutes per chat.
		if s.limiter.allow(chatID, true) {
			s.send(ctx, chatID, s.unlinkedText(), nil)
		}
		return
	}
	if !s.limiter.allow(chatID, false) {
		return
	}

	switch {
	case strings.HasPrefix(text, "/"):
		s.command(ctx, chatID, userID, text)
	case text != "":
		s.expense(ctx, chatID, userID, text)
	case m.Voice != nil || m.Audio != nil || len(m.Photo) > 0 || m.Document != nil:
		s.media(ctx, chatID, userID, m)
	}
}

// startCode reads "/start <code>" (also "/start@bot <code>").
func startCode(text string) (string, bool) {
	cmd, arg, _ := strings.Cut(text, " ")
	if cmd != "/start" && !strings.HasPrefix(cmd, "/start@") {
		return "", false
	}
	arg = strings.TrimSpace(arg)
	return arg, arg != ""
}

func (s *Service) link(ctx context.Context, chatID int64, code string) {
	userID, err := s.Queries.TakeTelegramLinkCode(ctx, sqlcgen.TakeTelegramLinkCodeParams{
		CodeHash: hashCode(code), Now: ts(s.now()),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		s.send(ctx, chatID, textBadCode, nil)
		return
	}
	if err != nil {
		s.fail(ctx, chatID, "take code", err)
		return
	}
	if err := s.Queries.UnlinkTelegramChat(ctx, chatID); err != nil {
		s.fail(ctx, chatID, "unlink chat", err)
		return
	}
	if err := s.Queries.LinkTelegram(ctx, sqlcgen.LinkTelegramParams{UserID: userID, ChatID: chatID}); err != nil {
		s.fail(ctx, chatID, "link", err)
		return
	}
	s.send(ctx, chatID, textLinked, nil)
}

func (s *Service) command(ctx context.Context, chatID int64, userID uuid.UUID, text string) {
	cmd, _, _ := strings.Cut(text, " ")
	cmd, _, _ = strings.Cut(cmd, "@")
	switch strings.ToLower(cmd) {
	case "/start", "/ayuda", "/help":
		s.send(ctx, chatID, textHelp, nil)
	case "/resumen":
		s.summary(ctx, chatID, userID)
	case "/comprometido":
		s.committed(ctx, chatID, userID)
	case "/deshacer":
		s.undo(ctx, chatID, userID)
	default:
		s.send(ctx, chatID, textHelp, nil)
	}
}

// ===================== drafts =====================

// option is a payment method offered as a button.
type option struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// draft is what waits for ✅, stored as JSON in telegram_drafts.fields.
type draft struct {
	HouseholdID  uuid.UUID  `json:"householdId"`
	Shared       bool       `json:"shared"`
	Amount       string     `json:"amount"`
	Currency     string     `json:"currency"`
	SpentAt      string     `json:"spentAt"` // YYYY-MM-DD
	Installments int        `json:"installments"`
	Description  string     `json:"description"`
	CategoryID   *uuid.UUID `json:"categoryId,omitempty"`
	Category     string     `json:"category,omitempty"`
	MethodID     *uuid.UUID `json:"methodId,omitempty"`
	Method       string     `json:"method,omitempty"`
	Options      []option   `json:"options,omitempty"`
	// Heard is what a voice note said or an image showed (shown above).
	Heard string `json:"heard,omitempty"`
}

func (s *Service) expense(ctx context.Context, chatID int64, userID uuid.UUID, text string) {
	personal, ok := s.begin(ctx, chatID, userID)
	if !ok {
		return
	}
	res, err := s.Parser.Parse(ctx, userID, personal.ID, text)
	if err != nil {
		s.fail(ctx, chatID, "parse", err)
		return
	}
	if !res.NotFinance && (!res.Understood || res.Amount <= 0) {
		if s.suspended(ctx, userID) {
			s.send(ctx, chatID, textAIPaused, nil)
		} else {
			s.send(ctx, chatID, textNoAmount, nil)
		}
		return
	}
	s.offer(ctx, chatID, userID, personal, res, "")
}

// media reads a voice note or a photo (plan 3.6, part 2) with the app's
// readers: up to 30 seconds of audio, images up to 5 MB.
func (s *Service) media(ctx context.Context, chatID int64, userID uuid.UUID, m *Message) {
	f, ok := pickFile(m)
	if !ok {
		s.send(ctx, chatID, textOnlyText, nil)
		return
	}
	if f.tooBig {
		s.send(ctx, chatID, f.tooBigText, nil)
		return
	}
	if s.suspended(ctx, userID) {
		s.send(ctx, chatID, textAIPaused, nil)
		return
	}
	personal, ok := s.begin(ctx, chatID, userID)
	if !ok {
		return
	}
	data, err := s.download(ctx, f)
	if err != nil {
		s.cfg.Logger.WarnContext(ctx, "telegram: download failed", "error", err)
		s.send(ctx, chatID, textCantRead, nil)
		return
	}
	var res expenseparse.Result
	if f.audio {
		res, err = s.Parser.ParseAudio(ctx, userID, personal.ID, f.mimeType, data)
	} else {
		res, err = s.Parser.ParseReceipt(ctx, userID, personal.ID, f.mimeType, data)
	}
	switch {
	case errors.Is(err, expenseparse.ErrAudioType) || errors.Is(err, expenseparse.ErrImageType):
		s.send(ctx, chatID, textOnlyText, nil)
		return
	case err != nil:
		s.fail(ctx, chatID, "parse media", err)
		return
	case !res.NotFinance && (!res.Understood || res.Amount <= 0):
		s.send(ctx, chatID, textCantRead, nil)
		return
	}
	heard := ""
	if res.Transcript != "" {
		if f.audio {
			heard = "Escuché: «" + res.Transcript + "»"
		} else {
			heard = "Leí: «" + res.Transcript + "»"
		}
	}
	s.offer(ctx, chatID, userID, personal, res, heard)
}

// begin replaces a draft waiting for a correction (✏️) and finds "Mi plata".
func (s *Service) begin(ctx context.Context, chatID int64, userID uuid.UUID) (domain.Household, bool) {
	if old, err := s.Queries.TelegramDraftBeingEdited(ctx, sqlcgen.TelegramDraftBeingEditedParams{
		UserID: userID, Since: ts(s.now().Add(-EditWindow)),
	}); err == nil {
		if _, err := s.Queries.DeleteTelegramDraft(ctx, sqlcgen.DeleteTelegramDraftParams{ID: old.ID, UserID: userID}); err == nil && old.MessageID.Valid {
			s.edit(ctx, chatID, old.MessageID.Int64, textReplaced, nil)
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		s.fail(ctx, chatID, "draft being edited", err)
		return domain.Household{}, false
	}
	personal, err := s.Spaces.PersonalForUser(ctx, userID)
	if err != nil {
		s.fail(ctx, chatID, "personal space", err)
		return domain.Household{}, false
	}
	return personal, true
}

// suspended says whether the person's AI is off (plan §8.6).
func (s *Service) suspended(ctx context.Context, userID uuid.UUID) bool {
	off, err := s.Queries.TelegramAISuspended(ctx, sqlcgen.TelegramAISuspendedParams{ID: userID, Now: ts(s.now())})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.cfg.Logger.WarnContext(ctx, "telegram: suspension lookup failed", "error", err)
	}
	return off
}

// offer saves the draft and sends it with its buttons. heard is what a
// voice note said or an image showed, shown above the expense.
func (s *Service) offer(ctx context.Context, chatID int64, userID uuid.UUID, personal domain.Household, res expenseparse.Result, heard string) {
	if res.NotFinance {
		s.send(ctx, chatID, textNotFinance, nil)
		return
	}
	d, err := s.newDraft(ctx, userID, personal, res)
	if err != nil {
		s.fail(ctx, chatID, "build draft", err)
		return
	}
	if d.MethodID == nil && len(d.Options) == 0 {
		s.send(ctx, chatID, textNoMethods, nil)
		return
	}
	d.Heard = heard
	fields, _ := json.Marshal(d)
	row, err := s.Queries.CreateTelegramDraft(ctx, sqlcgen.CreateTelegramDraftParams{UserID: userID, ChatID: chatID, Fields: fields})
	if err != nil {
		s.fail(ctx, chatID, "create draft", err)
		return
	}
	text, kb := s.card(row.ID, d)
	msgID, err := s.Bot.SendMessage(ctx, chatID, text, kb)
	if err != nil {
		s.cfg.Logger.WarnContext(ctx, "telegram: send draft failed", "error", err)
		return
	}
	if err := s.Queries.SetTelegramDraftMessage(ctx, sqlcgen.SetTelegramDraftMessageParams{
		ID: row.ID, MessageID: pgtype.Int8{Int64: msgID, Valid: true},
	}); err != nil {
		s.cfg.Logger.WarnContext(ctx, "telegram: keep message id failed", "error", err)
	}
}

// newDraft turns what was read into a draft: "Mi plata" unless the sentence
// says "compartido"/"hogar" and the person has a Hogar (as in the app).
func (s *Service) newDraft(ctx context.Context, userID uuid.UUID, personal domain.Household, res expenseparse.Result) (draft, error) {
	space := personal
	catID := res.CategoryID
	if res.Shared {
		hogar, err := s.Spaces.ActiveSharedForUser(ctx, userID)
		switch {
		case err == nil:
			space = hogar
			catID = uuid.Nil
			if s.Categorizer != nil && res.Description != "" {
				if sg, ok, err := s.Categorizer.Suggest(ctx, userID, hogar.ID, res.Description); err == nil && ok {
					catID = sg.CategoryID
				}
			}
		case !errors.Is(err, domain.ErrNotFound):
			return draft{}, err
		}
	}

	spent := s.today()
	if !res.SpentAt.IsZero() {
		spent = time.Date(res.SpentAt.Year(), res.SpentAt.Month(), res.SpentAt.Day(), 0, 0, 0, 0, time.UTC)
	}
	cur := res.Currency
	if cur == "" {
		cur = space.BaseCurrency
	}
	inst := res.Installments
	if inst < 1 {
		inst = 1
	}
	d := draft{
		HouseholdID: space.ID, Shared: space.ID != personal.ID,
		Amount: decimal.NewFromFloat(res.Amount).String(), Currency: cur, SpentAt: spent.Format(time.DateOnly),
		Installments: inst, Description: res.Description,
	}
	if catID != uuid.Nil {
		d.CategoryID = &catID
		if s.CategoryName != nil {
			if name, err := s.CategoryName(ctx, catID); err == nil {
				d.Category = name
			}
		}
	}
	if d.Description == "" {
		d.Description = d.Category
	}
	if d.Description == "" {
		d.Description = "Gasto"
	}

	methods, err := s.Queries.TelegramMethods(ctx, sqlcgen.TelegramMethodsParams{
		OwnerUserID: userID, Since: ts(s.now().Add(-methodUsageWindow)),
	})
	if err != nil {
		return draft{}, err
	}
	if res.PaymentMethodID != uuid.Nil {
		for _, m := range methods {
			if m.ID == res.PaymentMethodID {
				id := m.ID
				d.MethodID, d.Method = &id, m.Name
			}
		}
	}
	if d.MethodID == nil {
		d.Options = methodOptions(methods, res.Candidates)
	}
	return d, nil
}

// methodOptions: the candidates the words fit first, then the most used,
// up to MaxMethodButtons.
func methodOptions(methods []sqlcgen.TelegramMethodsRow, candidates []uuid.UUID) []option {
	out := make([]option, 0, MaxMethodButtons)
	used := map[uuid.UUID]bool{}
	for _, c := range candidates {
		for _, m := range methods {
			if m.ID == c && !used[c] && len(out) < MaxMethodButtons {
				out = append(out, option{ID: m.ID, Name: m.Name})
				used[c] = true
			}
		}
	}
	for _, m := range methods {
		if !used[m.ID] && len(out) < MaxMethodButtons {
			out = append(out, option{ID: m.ID, Name: m.Name})
			used[m.ID] = true
		}
	}
	return out
}

// card is the draft's message and buttons: the method buttons while no
// method is picked, then ✅ ✏️ ❌.
func (s *Service) card(id uuid.UUID, d draft) (string, Keyboard) {
	sid := id.String()
	if d.MethodID == nil {
		kb := Keyboard{}
		for i := 0; i < len(d.Options); i += 2 {
			row := []InlineButton{{Text: d.Options[i].Name, CallbackData: cbMethod(sid, i)}}
			if i+1 < len(d.Options) {
				row = append(row, InlineButton{Text: d.Options[i+1].Name, CallbackData: cbMethod(sid, i+1)})
			}
			kb = append(kb, row)
		}
		kb = append(kb, []InlineButton{{Text: "✏️ Corregir", CallbackData: "ed:" + sid}, {Text: "❌ Cancelar", CallbackData: "no:" + sid}})
		return heardLine(d) + s.summary1(d) + "\n\n¿Con qué pagaste?", kb
	}
	return heardLine(d) + s.summary1(d) + "\n\n¿Lo guardo?", Keyboard{{
		{Text: "✅ Guardar", CallbackData: "ok:" + sid},
		{Text: "✏️ Corregir", CallbackData: "ed:" + sid},
		{Text: "❌ Cancelar", CallbackData: "no:" + sid},
	}}
}

// cbMethod is "pm:<draft>:<i>"; i is one digit (MaxMethodButtons < 10).
func cbMethod(id string, i int) string { return "pm:" + id + ":" + strconv.Itoa(i) }

// ===================== buttons =====================

func (s *Service) callback(ctx context.Context, q *CallbackQuery) {
	answer := func(text string) {
		if err := s.Bot.AnswerCallback(ctx, q.ID, text); err != nil {
			s.cfg.Logger.WarnContext(ctx, "telegram: answer callback failed", "error", err)
		}
	}
	if q.Message == nil || q.Message.Chat.Type != "private" || q.Message.Chat.ID != q.From.ID {
		answer("")
		return
	}
	chatID, msgID := q.Message.Chat.ID, q.Message.MessageID
	userID, ok := s.user(ctx, chatID)
	if !ok {
		answer(s.unlinkedText())
		return
	}
	if !s.limiter.allow(chatID, false) {
		answer("")
		return
	}
	action, rest, _ := strings.Cut(q.Data, ":")
	idText, arg, _ := strings.Cut(rest, ":")
	id, err := uuid.Parse(idText)
	if err != nil {
		answer("")
		return
	}
	row, err := s.Queries.GetTelegramDraft(ctx, sqlcgen.GetTelegramDraftParams{ID: id, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		answer(textGone)
		s.edit(ctx, chatID, msgID, textGone, nil)
		return
	}
	if err != nil {
		answer(textError)
		s.cfg.Logger.ErrorContext(ctx, "telegram: get draft failed", "error", err)
		return
	}
	var d draft
	if err := json.Unmarshal(row.Fields, &d); err != nil {
		answer(textError)
		return
	}
	if row.SavedAt.Valid {
		answer(textAlreadySaved)
		return
	}

	switch action {
	case "pm":
		if len(arg) != 1 || arg[0] < '0' || int(arg[0]-'0') >= len(d.Options) {
			answer("")
			return
		}
		i := int(arg[0] - '0')
		mid := d.Options[i].ID
		d.MethodID, d.Method, d.Options = &mid, d.Options[i].Name, nil
		fields, _ := json.Marshal(d)
		if err := s.Queries.UpdateTelegramDraftFields(ctx, sqlcgen.UpdateTelegramDraftFieldsParams{ID: id, UserID: userID, Fields: fields}); err != nil {
			answer(textError)
			return
		}
		answer("")
		text, kb := s.card(id, d)
		s.edit(ctx, chatID, msgID, text, kb)
	case "ok":
		s.save(ctx, chatID, msgID, userID, id, d, answer)
	case "ed":
		if _, err := s.Queries.EditTelegramDraft(ctx, sqlcgen.EditTelegramDraftParams{ID: id, UserID: userID, Now: ts(s.now())}); err != nil {
			answer(textError)
			return
		}
		if err := s.Queries.ClearTelegramEditing(ctx, sqlcgen.ClearTelegramEditingParams{UserID: userID, Keep: id}); err != nil {
			s.cfg.Logger.WarnContext(ctx, "telegram: clear editing failed", "error", err)
		}
		answer("")
		s.edit(ctx, chatID, msgID, s.summary1(d)+"\n\n"+textAskCorrection, Keyboard{{{Text: "❌ Cancelar", CallbackData: "no:" + id.String()}}})
	case "no":
		if _, err := s.Queries.DeleteTelegramDraft(ctx, sqlcgen.DeleteTelegramDraftParams{ID: id, UserID: userID}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			answer(textError)
			return
		}
		answer("")
		s.edit(ctx, chatID, msgID, "❌ Cancelado\n"+s.summary1(d), nil)
	default:
		answer("")
	}
}

func (s *Service) save(ctx context.Context, chatID, msgID int64, userID, id uuid.UUID, d draft, answer func(string)) {
	if d.MethodID == nil {
		answer(textPickMethod)
		return
	}
	amount, err := decimal.NewFromString(d.Amount)
	if err != nil {
		answer(textError)
		return
	}
	spent, err := time.Parse(time.DateOnly, d.SpentAt)
	if err != nil {
		answer(textError)
		return
	}
	if _, err := s.Queries.ClaimTelegramDraft(ctx, sqlcgen.ClaimTelegramDraftParams{ID: id, UserID: userID, Now: ts(s.now())}); err != nil {
		answer(textAlreadySaved) // a second tap
		return
	}
	exp, err := s.Expenses.Create(ctx, expenses.CreateInput{
		HouseholdID: d.HouseholdID, CreatedBy: userID, CategoryID: d.CategoryID, PaymentMethodID: *d.MethodID,
		Amount: amount, Currency: d.Currency, Description: d.Description, SpentAt: spent,
		Installments: d.Installments, IsShared: d.Shared,
	})
	if err != nil {
		if uerr := s.Queries.UnclaimTelegramDraft(ctx, id); uerr != nil {
			s.cfg.Logger.ErrorContext(ctx, "telegram: unclaim failed", "error", uerr)
		}
		s.cfg.Logger.WarnContext(ctx, "telegram: save expense failed", "error", err)
		answer(textSaveFailed)
		return
	}
	eid := exp.Expense.ID
	if err := s.Queries.SetTelegramDraftExpense(ctx, sqlcgen.SetTelegramDraftExpenseParams{ID: id, ExpenseID: &eid}); err != nil {
		s.cfg.Logger.ErrorContext(ctx, "telegram: keep expense id failed", "error", err)
	}
	answer("Guardado")
	s.edit(ctx, chatID, msgID, "✅ Guardado\n"+s.summary1(d), nil)
}

// ===================== commands =====================

func (s *Service) undo(ctx context.Context, chatID int64, userID uuid.UUID) {
	row, err := s.Queries.LastTelegramExpense(ctx, sqlcgen.LastTelegramExpenseParams{
		UserID: userID, Since: ts(s.now().Add(-UndoWindow)),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		s.send(ctx, chatID, textNothingToUndo, nil)
		return
	}
	if err != nil {
		s.fail(ctx, chatID, "last expense", err)
		return
	}
	var d draft
	_ = json.Unmarshal(row.Fields, &d)
	if err := s.Expenses.Delete(ctx, d.HouseholdID, *row.ExpenseID, userID); err != nil && !errors.Is(err, domain.ErrNotFound) {
		s.fail(ctx, chatID, "undo", err)
		return
	}
	if err := s.Queries.ForgetTelegramDraft(ctx, row.ID); err != nil {
		s.cfg.Logger.WarnContext(ctx, "telegram: forget draft failed", "error", err)
	}
	if row.MessageID.Valid {
		s.edit(ctx, chatID, row.MessageID.Int64, "↩️ Deshecho\n"+s.summary1(d), nil)
	}
	s.send(ctx, chatID, "↩️ Borré «"+d.Description+" · "+formatAmount(d)+"».", nil)
}

func (s *Service) summary(ctx context.Context, chatID int64, userID uuid.UUID) {
	personal, err := s.Spaces.PersonalForUser(ctx, userID)
	if err != nil {
		s.fail(ctx, chatID, "personal space", err)
		return
	}
	row, err := s.Queries.LatestWeeklyReview(ctx, personal.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		s.send(ctx, chatID, textNoSummary, nil)
		return
	}
	if err != nil {
		s.fail(ctx, chatID, "weekly review", err)
		return
	}
	s.send(ctx, chatID, row.Title+"\n\n"+row.Body, nil)
}

func (s *Service) committed(ctx context.Context, chatID int64, userID uuid.UUID) {
	c, err := s.Committer.Committed(ctx, userID, CommittedMonths)
	if err != nil {
		s.fail(ctx, chatID, "committed", err)
		return
	}
	s.send(ctx, chatID, committedText(c), nil)
}

// ===================== helpers =====================

func (s *Service) send(ctx context.Context, chatID int64, text string, kb Keyboard) {
	if _, err := s.Bot.SendMessage(ctx, chatID, text, kb); err != nil {
		s.cfg.Logger.WarnContext(ctx, "telegram: send failed", "error", err)
	}
}

func (s *Service) edit(ctx context.Context, chatID, msgID int64, text string, kb Keyboard) {
	if err := s.Bot.EditMessage(ctx, chatID, msgID, text, kb); err != nil {
		s.cfg.Logger.WarnContext(ctx, "telegram: edit failed", "error", err)
	}
}

func (s *Service) fail(ctx context.Context, chatID int64, step string, err error) {
	s.cfg.Logger.ErrorContext(ctx, "telegram: "+step+" failed", "error", err)
	s.send(ctx, chatID, textError, nil)
}

// ===================== limits =====================

// chatLimiter: per chat, at most 20 updates a minute (plan §8.2.3), and the
// reply to an unlinked chat at most once every 10 minutes.
type chatLimiter struct {
	mu       sync.Mutex
	now      func() time.Time
	windows  map[int64][]time.Time
	unlinked map[int64]time.Time
}

const (
	chatPerMinute  = 20
	unlinkedRepeat = 10 * time.Minute
	limiterMaxKeys = 10000
)

func newChatLimiter(now func() time.Time) *chatLimiter {
	return &chatLimiter{now: now, windows: map[int64][]time.Time{}, unlinked: map[int64]time.Time{}}
}

func (l *chatLimiter) allow(chatID int64, unlinked bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.windows) > limiterMaxKeys {
		l.windows = map[int64][]time.Time{}
	}
	if len(l.unlinked) > limiterMaxKeys {
		l.unlinked = map[int64]time.Time{}
	}
	if unlinked {
		if last, ok := l.unlinked[chatID]; ok && now.Sub(last) < unlinkedRepeat {
			return false
		}
		l.unlinked[chatID] = now
		return true
	}
	recent := l.windows[chatID][:0]
	for _, t := range l.windows[chatID] {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	if len(recent) >= chatPerMinute {
		l.windows[chatID] = recent
		return false
	}
	l.windows[chatID] = append(recent, now)
	return true
}

// recentIDs remembers the last update IDs, so a resent update is handled once.
type recentIDs struct {
	mu   sync.Mutex
	set  map[int64]bool
	ring []int64
	next int
}

func newRecentIDs(n int) *recentIDs {
	return &recentIDs{set: make(map[int64]bool, n), ring: make([]int64, n)}
}

func (r *recentIDs) add(id int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.set[id] {
		return false
	}
	if old := r.ring[r.next]; old != 0 {
		delete(r.set, old)
	}
	r.ring[r.next] = id
	r.next = (r.next + 1) % len(r.ring)
	r.set[id] = true
	return true
}

// Setup registers the webhook and the command menu with Telegram (on start).
type Setupper interface {
	Setup(ctx context.Context, url, secret string, cmds []Command) error
}

// Starter registers the bot in the background on start; a failure is
// logged and the API keeps running (the previous registration stays).
func Starter(c Setupper, webhookURL, secret string, logger *slog.Logger) func(context.Context) func() {
	return func(ctx context.Context) func() {
		ctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			sctx, scancel := context.WithTimeout(ctx, 30*time.Second)
			defer scancel()
			if err := c.Setup(sctx, webhookURL, secret, Commands); err != nil {
				logger.ErrorContext(ctx, "telegram: webhook setup failed", "error", err)
				return
			}
			logger.InfoContext(ctx, "telegram: webhook registered")
		}()
		return func() { cancel(); <-done }
	}
}
