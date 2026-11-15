package telegram_test

// The Telegram bot (plan 3.6) against the real API and Postgres, with a fake
// Bot API server that records every call.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LucianoR23/valuo_api/internal/config"
	"github.com/LucianoR23/valuo_api/internal/telegram"
	"github.com/LucianoR23/valuo_api/internal/testutil/apitest"
	"github.com/LucianoR23/valuo_api/internal/testutil/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

const (
	token  = "123:test-token"
	secret = "test-secret-test-secret-test-secret-1234"
)

type call struct {
	Method string
	Params map[string]any
}

// fakeBot answers the Bot API like Telegram and keeps what it was asked.
type fakeBot struct {
	mu    sync.Mutex
	calls []call
	next  int64
}

// files are what the fake serves for download, by file_id.
var files = map[string][]byte{
	"voice-1": []byte("OggS voice note"),
	"big":     append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte("jpeg ticket")...),
}

func (f *fakeBot) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if data, ok := strings.CutPrefix(r.URL.Path, "/file/bot"+token+"/f/"); ok {
		f.mu.Lock()
		f.calls = append(f.calls, call{Method: "download", Params: map[string]any{"path": data}})
		f.mu.Unlock()
		_, _ = w.Write(files[data])
		return
	}
	prefix := "/bot" + token + "/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	var p map[string]any
	_ = json.NewDecoder(r.Body).Decode(&p)
	f.mu.Lock()
	method := strings.TrimPrefix(r.URL.Path, prefix)
	f.calls = append(f.calls, call{Method: method, Params: p})
	f.next++
	id := f.next
	f.mu.Unlock()
	var result any = true
	switch method {
	case "sendMessage":
		result = map[string]int64{"message_id": id}
	case "getFile":
		result = map[string]string{"file_path": "f/" + p["file_id"].(string)}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

// take returns and forgets the calls so far.
func (f *fakeBot) take() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	return out
}

func text(c call) string {
	s, _ := c.Params["text"].(string)
	return s
}

// buttons lists the callback data of a call's keyboard.
func buttons(c call) []string {
	var out []string
	rm, _ := c.Params["reply_markup"].(map[string]any)
	rows, _ := rm["inline_keyboard"].([]any)
	for _, row := range rows {
		for _, b := range row.([]any) {
			out = append(out, b.(map[string]any)["callback_data"].(string))
		}
	}
	return out
}

func button(t *testing.T, c call, prefix string) string {
	t.Helper()
	for _, b := range buttons(c) {
		if strings.HasPrefix(b, prefix) {
			return b
		}
	}
	t.Fatalf("no %q button in %v", prefix, buttons(c))
	return ""
}

func one(t *testing.T, calls []call, method string) call {
	t.Helper()
	var found []call
	for _, c := range calls {
		if c.Method == method {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want one %s, got %d: %+v", method, len(found), calls)
	}
	return found[0]
}

type bot struct {
	t      *testing.T
	s      *apitest.Server
	fake   *fakeBot
	chat   int64
	update int64
}

func (b *bot) post(u map[string]any, wantStatus int) []call {
	b.t.Helper()
	b.update++
	u["update_id"] = b.update
	b.s.Do(b.t, apitest.Req{Method: http.MethodPost, Path: "/telegram/webhook", Body: u,
		Header: http.Header{telegram.SecretHeader: {secret}}}).Status(b.t, wantStatus)
	return b.fake.take()
}

func (b *bot) say(msg string) []call {
	b.t.Helper()
	return b.post(map[string]any{"message": map[string]any{
		"message_id": 1, "from": map[string]any{"id": b.chat},
		"chat": map[string]any{"id": b.chat, "type": "private"}, "text": msg,
	}}, http.StatusOK)
}

func (b *bot) send(msg map[string]any) []call {
	b.t.Helper()
	msg["message_id"] = 1
	msg["from"] = map[string]any{"id": b.chat}
	msg["chat"] = map[string]any{"id": b.chat, "type": "private"}
	return b.post(map[string]any{"message": msg}, http.StatusOK)
}

func (b *bot) tap(data string, msgID float64) []call {
	b.t.Helper()
	return b.post(map[string]any{"callback_query": map[string]any{
		"id": "cb", "from": map[string]any{"id": b.chat}, "data": data,
		"message": map[string]any{"message_id": msgID, "chat": map[string]any{"id": b.chat, "type": "private"}},
	}}, http.StatusOK)
}

// sent is the message_id the fake gave the last sendMessage of calls.
func sentID(calls []call, f *fakeBot) float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return float64(f.next)
}

func expenseCount(t *testing.T, s *apitest.Server, userID string) int {
	t.Helper()
	var n int
	if err := s.Pool.QueryRow(context.Background(), `SELECT count(*) FROM expenses WHERE created_by = $1`, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestTelegramBot(t *testing.T) {
	t.Parallel()
	fake := &fakeBot{}
	tg := httptest.NewServer(fake)
	t.Cleanup(tg.Close)
	now := time.Date(2026, 11, 14, 15, 0, 0, 0, time.UTC)
	s := apitest.New(t, apitest.WithNow(func() time.Time { return now }), apitest.WithConfig(func(c *config.Config) {
		c.TelegramEnabled = true
		c.TelegramBotToken = token
		c.TelegramWebhookSecret = secret
		c.TelegramBotUsername = "test_bot"
		c.TelegramAPIBaseURL = tg.URL
	}))

	unverified := s.Register(t)
	s.Do(t, apitest.Req{Method: http.MethodPost, Path: "/telegram/link", Token: unverified.Token}).Status(t, http.StatusForbidden)

	u := s.Register(t, apitest.RegisterOpts{Verify: true})
	visa := s.CreateCreditCard(t, u, "Visa Galicia", 25, 5, "2026-11-25", "2026-12-05")
	b := &bot{t: t, s: s, fake: fake, chat: 4242}

	t.Run("webhook needs the secret", func(t *testing.T) {
		body := map[string]any{"update_id": 1}
		s.Do(t, apitest.Req{Method: http.MethodPost, Path: "/telegram/webhook", Body: body}).Status(t, http.StatusUnauthorized)
		s.Do(t, apitest.Req{Method: http.MethodPost, Path: "/telegram/webhook", Body: body,
			Header: http.Header{telegram.SecretHeader: {"wrong"}}}).Status(t, http.StatusUnauthorized)
		if calls := fake.take(); len(calls) != 0 {
			t.Errorf("calls %+v", calls)
		}
	})

	t.Run("an unlinked chat gets the fixed reply once", func(t *testing.T) {
		c := one(t, b.say("café 2500"), "sendMessage")
		if !strings.Contains(text(c), "conectá tu cuenta de TestBrand") {
			t.Errorf("reply %q", text(c))
		}
		if calls := b.say("hola?"); len(calls) != 0 {
			t.Errorf("second reply: %+v", calls)
		}
	})

	var link struct {
		URL       string    `json:"url"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	s.Do(t, apitest.Req{Method: http.MethodPost, Path: "/telegram/link", Token: u.Token}).Status(t, http.StatusCreated).JSON(t, &link)
	code, ok := strings.CutPrefix(link.URL, "https://t.me/test_bot?start=")
	if !ok || len(code) != 32 || !link.ExpiresAt.Equal(now.Add(telegram.CodeTTL)) {
		t.Fatalf("link %+v", link)
	}

	t.Run("start with the code links the chat, once", func(t *testing.T) {
		if c := one(t, b.say("/start "+code), "sendMessage"); !strings.HasPrefix(text(c), "Listo") {
			t.Errorf("reply %q", text(c))
		}
		var st struct {
			Linked bool `json:"linked"`
		}
		s.Do(t, apitest.Req{Method: http.MethodGet, Path: "/telegram/link", Token: u.Token}).Status(t, http.StatusOK).JSON(t, &st)
		if !st.Linked {
			t.Error("not linked")
		}
		if c := one(t, b.say("/start "+code), "sendMessage"); !strings.Contains(text(c), "venció o ya se usó") {
			t.Errorf("reused code: %q", text(c))
		}
	})

	t.Run("no method said: pick one, then save", func(t *testing.T) {
		c := one(t, b.say("café 2500"), "sendMessage")
		if !strings.Contains(text(c), "Café · $ 2.500") || !strings.Contains(text(c), "¿Con qué pagaste?") {
			t.Fatalf("draft %q", text(c))
		}
		msg := sentID(nil, fake)
		// Efectivo and Visa Galicia, never used: alphabetical.
		pick := button(t, c, "pm:")
		edit := one(t, b.tap(pick, msg), "editMessageText")
		if !strings.Contains(text(edit), "Efectivo") || !strings.Contains(text(edit), "¿Lo guardo?") {
			t.Fatalf("after pick %q", text(edit))
		}
		ok := button(t, edit, "ok:")
		saved := one(t, b.tap(ok, msg), "editMessageText")
		if !strings.HasPrefix(text(saved), "✅ Guardado") || len(buttons(saved)) != 0 {
			t.Errorf("saved %q %v", text(saved), buttons(saved))
		}
		if n := expenseCount(t, s, u.ID); n != 1 {
			t.Fatalf("expenses %d", n)
		}
		again := one(t, b.tap(ok, msg), "answerCallbackQuery")
		if again.Params["text"] != "Ya está guardado." || expenseCount(t, s, u.ID) != 1 {
			t.Errorf("second tap %+v", again.Params)
		}
	})

	t.Run("the same expense again: the draft warns (plan 3.9)", func(t *testing.T) {
		c := one(t, b.say("café 2500"), "sendMessage")
		if !strings.Contains(text(c), "⚠️ Parecido a uno que ya está: Café · $ 2.500 · hoy\n") {
			t.Fatalf("draft %q", text(c))
		}
		// Left unsaved: the chat allows 20 updates a minute and the clock stands still.
		if n := expenseCount(t, s, u.ID); n != 1 {
			t.Fatalf("expenses %d", n)
		}
	})

	t.Run("correct with the pencil, then cancel", func(t *testing.T) {
		c := one(t, b.say("super 15000 visa galicia"), "sendMessage")
		if !strings.Contains(text(c), "Visa Galicia") || !strings.Contains(text(c), "¿Lo guardo?") {
			t.Fatalf("draft %q", text(c))
		}
		first := sentID(nil, fake)
		ed := one(t, b.tap(button(t, c, "ed:"), first), "editMessageText")
		if !strings.Contains(text(ed), "Mandame la frase corregida") {
			t.Errorf("pencil %q", text(ed))
		}
		calls := b.say("super 16000 visa galicia")
		if r := one(t, calls, "editMessageText"); !strings.Contains(text(r), "Corregido") || r.Params["message_id"] != first {
			t.Errorf("old draft %+v", r.Params)
		}
		fixed := one(t, calls, "sendMessage")
		if !strings.Contains(text(fixed), "$ 16.000") {
			t.Errorf("corrected %q", text(fixed))
		}
		no := one(t, b.tap(button(t, fixed, "no:"), sentID(nil, fake)), "editMessageText")
		if !strings.HasPrefix(text(no), "❌ Cancelado") || expenseCount(t, s, u.ID) != 1 {
			t.Errorf("cancel %q", text(no))
		}
		gone := one(t, b.tap(button(t, fixed, "ok:"), sentID(nil, fake)), "answerCallbackQuery")
		if gone.Params["text"] != "Ese gasto ya no está para confirmar." {
			t.Errorf("tap after cancel %+v", gone.Params)
		}
	})

	t.Run("shared goes to the Hogar", func(t *testing.T) {
		hogar := s.CreateHousehold(t, u, "Casa")
		c := one(t, b.say("pizza 9000 visa galicia 3 cuotas compartido"), "sendMessage")
		if !strings.Contains(text(c), "3 cuotas") || !strings.Contains(text(c), "Hogar") {
			t.Fatalf("draft %q", text(c))
		}
		one(t, b.tap(button(t, c, "ok:"), sentID(nil, fake)), "editMessageText")
		var hh, pm string
		var inst int
		if err := s.Pool.QueryRow(context.Background(), `SELECT household_id::text, payment_method_id::text, installments
			FROM expenses WHERE created_by = $1 AND description = 'Pizza'`, u.ID).Scan(&hh, &pm, &inst); err != nil {
			t.Fatal(err)
		}
		if hh != hogar.ID || pm != visa.ID || inst != 3 {
			t.Errorf("expense in %s with %s, %d installments", hh, pm, inst)
		}
	})

	t.Run("deshacer removes the last one", func(t *testing.T) {
		calls := b.say("/deshacer")
		if c := one(t, calls, "sendMessage"); !strings.Contains(text(c), "Borré «Pizza") {
			t.Errorf("undo %q", text(c))
		}
		if n := expenseCount(t, s, u.ID); n != 1 {
			t.Errorf("expenses %d", n)
		}
	})

	t.Run("commands", func(t *testing.T) {
		if c := one(t, b.say("/resumen"), "sendMessage"); text(c) != "Todavía no tenés un resumen semanal." {
			t.Errorf("resumen %q", text(c))
		}
		if c := one(t, b.say("/comprometido"), "sendMessage"); !strings.Contains(text(c), "No tenés nada comprometido en tarjetas.") {
			t.Errorf("comprometido %q", text(c))
		}
		if c := one(t, b.say("/ayuda"), "sendMessage"); !strings.Contains(text(c), "/deshacer") {
			t.Errorf("ayuda %q", text(c))
		}
		if c := one(t, b.say("escribime un poema"), "sendMessage"); !strings.Contains(text(c), "No encontré el monto") {
			t.Errorf("no amount %q", text(c))
		}
	})

	t.Run("another account cannot use these buttons", func(t *testing.T) {
		c := one(t, b.say("taxi 4000 efectivo"), "sendMessage")
		other := &bot{t: t, s: s, fake: fake, chat: 777, update: b.update + 100}
		reply := one(t, other.tap(button(t, c, "ok:"), sentID(nil, fake)), "answerCallbackQuery")
		if !strings.Contains(reply.Params["text"].(string), "conectá tu cuenta") {
			t.Errorf("stranger %+v", reply.Params)
		}
		b.update = other.update
	})

	t.Run("unlink tells the chat", func(t *testing.T) {
		s.Do(t, apitest.Req{Method: http.MethodDelete, Path: "/telegram/link", Token: u.Token}).Status(t, http.StatusNoContent)
		if c := one(t, fake.take(), "sendMessage"); !strings.HasPrefix(text(c), "Desconectaste") {
			t.Errorf("bye %q", text(c))
		}
		var st struct {
			Linked bool `json:"linked"`
		}
		s.Do(t, apitest.Req{Method: http.MethodGet, Path: "/telegram/link", Token: u.Token}).Status(t, http.StatusOK).JSON(t, &st)
		if st.Linked {
			t.Error("still linked")
		}
	})

	t.Run("cleanup deletes day-old drafts and expired codes", func(t *testing.T) {
		ctx := context.Background()
		s.Do(t, apitest.Req{Method: http.MethodPost, Path: "/telegram/link", Token: u.Token}).Status(t, http.StatusCreated)
		if n, err := s.Jobs.TelegramCleanup(ctx); err != nil || n != 0 {
			t.Fatalf("fresh rows: %d %v", n, err)
		}
		if _, err := s.Pool.Exec(ctx, `UPDATE telegram_drafts SET created_at = $1`, now.Add(-25*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Pool.Exec(ctx, `UPDATE telegram_link_codes SET expires_at = $1`, now.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		if n, err := s.Jobs.TelegramCleanup(ctx); err != nil || n < 2 {
			t.Fatalf("old rows: %d %v", n, err)
		}
		var left int
		_ = s.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM telegram_drafts) + (SELECT count(*) FROM telegram_link_codes)`).Scan(&left)
		if left != 0 {
			t.Errorf("left %d", left)
		}
	})
}

func TestTelegramOff(t *testing.T) {
	t.Parallel()
	s := apitest.New(t)
	u := s.Register(t, apitest.RegisterOpts{Verify: true})
	s.Do(t, apitest.Req{Method: http.MethodPost, Path: "/telegram/link", Token: u.Token}).Status(t, http.StatusServiceUnavailable)
	s.Do(t, apitest.Req{Method: http.MethodPost, Path: "/telegram/webhook", Body: map[string]any{"update_id": 1},
		Header: http.Header{telegram.SecretHeader: {secret}}}).Status(t, http.StatusServiceUnavailable)
}
