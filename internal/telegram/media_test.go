package telegram_test

// Voice notes, photos and the automatic AI suspension through the bot
// (plan 3.6, part 2), with fake Claude and Gemini servers.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LucianoR23/valuo_api/internal/config"
	"github.com/LucianoR23/valuo_api/internal/testutil/apitest"
)

// fakeAI answers like Claude's Messages API or Gemini's Interactions API,
// with the answer set by the test, and keeps the media types it got.
type fakeAI struct {
	mu     sync.Mutex
	gemini bool
	answer string
	media  []string
}

func (f *fakeAI) set(answer string) {
	f.mu.Lock()
	f.answer = answer
	f.mu.Unlock()
}

func (f *fakeAI) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.media...)
}

func (f *fakeAI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req map[string]any
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	answer := f.answer
	if f.gemini {
		inputs, _ := req["input"].([]any)
		for _, p := range inputs {
			if mt, ok := p.(map[string]any)["mime_type"].(string); ok {
				f.media = append(f.media, mt)
			}
		}
	} else {
		msgs, _ := req["messages"].([]any)
		for _, m := range msgs {
			parts, _ := m.(map[string]any)["content"].([]any)
			for _, p := range parts {
				if src, ok := p.(map[string]any)["source"].(map[string]any); ok {
					f.media = append(f.media, src["media_type"].(string))
				}
			}
		}
	}
	f.mu.Unlock()
	var out any
	if f.gemini {
		out = map[string]any{
			"status": "completed",
			"steps":  []map[string]any{{"type": "model_output", "content": []map[string]string{{"type": "text", "text": answer}}}},
			"usage": map[string]any{"total_input_tokens": 300, "total_output_tokens": 60,
				"input_tokens_by_modality": []map[string]any{{"modality": "text", "tokens": 140}, {"modality": "audio", "tokens": 160}}},
		}
	} else {
		out = map[string]any{
			"content":     []map[string]string{{"type": "text", "text": answer}},
			"stop_reason": "end_turn",
			"usage":       map[string]int{"input_tokens": 900, "output_tokens": 40},
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func TestTelegramVoicePhotoAndSuspension(t *testing.T) {
	t.Parallel()
	fake := &fakeBot{}
	tg := httptest.NewServer(fake)
	t.Cleanup(tg.Close)
	claude, gemini := &fakeAI{}, &fakeAI{gemini: true}
	cs, gs := httptest.NewServer(claude), httptest.NewServer(gemini)
	t.Cleanup(cs.Close)
	t.Cleanup(gs.Close)
	now := time.Date(2026, 11, 14, 15, 0, 0, 0, time.UTC)
	s := apitest.New(t, apitest.WithNow(func() time.Time { return now }), apitest.WithConfig(func(c *config.Config) {
		c.TelegramEnabled = true
		c.TelegramBotToken = token
		c.TelegramWebhookSecret = secret
		c.TelegramBotUsername = "test_bot"
		c.TelegramAPIBaseURL = tg.URL
		c.AIEnabled = true
		c.AnthropicAPIKey, c.AnthropicBaseURL = "sk-test", cs.URL
		c.GeminiAPIKey, c.GeminiBaseURL = "g-test", gs.URL
		c.AIUserPerMinute = 100 // the clock stands still
	}))
	u := s.Register(t, apitest.RegisterOpts{Verify: true})
	b := &bot{t: t, s: s, fake: fake, chat: 5151}
	var link struct {
		URL string `json:"url"`
	}
	s.Do(t, apitest.Req{Method: http.MethodPost, Path: "/telegram/link", Token: u.Token}).Status(t, http.StatusCreated).JSON(t, &link)
	b.say("/start " + strings.TrimPrefix(link.URL, "https://t.me/test_bot?start="))

	usage := func() string {
		rows, err := s.Pool.Query(context.Background(), `SELECT task || ':' || channel || ':' || status FROM ai_usage
			WHERE task <> 'categorize' ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				t.Fatal(err)
			}
			out = append(out, v)
		}
		return strings.Join(out, ",")
	}

	t.Run("a voice note", func(t *testing.T) {
		gemini.set(`{"intent":"expense","transcript":"taxi 4000 en efectivo","amount":4000,"currency":"","date":"",` +
			`"installments":0,"payment":"efectivo","description":"taxi","shared":false}`)
		calls := b.send(map[string]any{"voice": map[string]any{"file_id": "voice-1", "duration": 6, "mime_type": "audio/ogg", "file_size": 20}})
		c := one(t, calls, "sendMessage")
		if !strings.HasPrefix(text(c), "Escuché: «taxi 4000 en efectivo»") || !strings.Contains(text(c), "Taxi · $ 4.000\nEfectivo") {
			t.Fatalf("card %q", text(c))
		}
		one(t, calls, "download")
		if m := gemini.got(); len(m) != 1 || m[0] != "audio/ogg" {
			t.Errorf("gemini got %v", m)
		}
		one(t, b.tap(button(t, c, "ok:"), sentID(nil, fake)), "editMessageText")
		if n := expenseCount(t, s, u.ID); n != 1 {
			t.Errorf("expenses %d", n)
		}
	})

	t.Run("a voice note over 30 seconds is not downloaded", func(t *testing.T) {
		calls := b.send(map[string]any{"voice": map[string]any{"file_id": "voice-1", "duration": 31, "file_size": 20}})
		if c := one(t, calls, "sendMessage"); !strings.Contains(text(c), "hasta 30 segundos") || len(calls) != 1 {
			t.Errorf("calls %+v", calls)
		}
	})

	t.Run("a photo: the largest size under the cap is read", func(t *testing.T) {
		claude.set(`{"intent":"expense","amount":15430.5,"currency":"","date":"2026-11-13","installments":0,` +
			`"payment":"efectivo","description":"Carrefour"}`)
		calls := b.send(map[string]any{"photo": []map[string]any{
			{"file_id": "small", "width": 90, "height": 160, "file_size": 2000},
			{"file_id": "big", "width": 720, "height": 1280, "file_size": 90000},
			{"file_id": "huge", "width": 4000, "height": 6000, "file_size": 6 << 20},
		}})
		c := one(t, calls, "sendMessage")
		if !strings.HasPrefix(text(c), "Leí: «") || !strings.Contains(text(c), "Carrefour · $ 15.430,50\nEfectivo · ayer") {
			t.Fatalf("card %q", text(c))
		}
		if d := one(t, calls, "download"); d.Params["path"] != "big" {
			t.Errorf("downloaded %v", d.Params)
		}
		if m := claude.got(); len(m) != 1 || m[0] != "image/jpeg" {
			t.Errorf("claude got %v", m)
		}
	})

	t.Run("anything else gets the fixed reply", func(t *testing.T) {
		calls := b.send(map[string]any{"document": map[string]any{"file_id": "doc", "mime_type": "application/pdf", "file_size": 10}})
		if c := one(t, calls, "sendMessage"); !strings.HasPrefix(text(c), "Puedo leer texto, notas de voz y fotos") {
			t.Errorf("reply %q", text(c))
		}
	})

	t.Run("five off-topic messages pause the AI", func(t *testing.T) {
		claude.set(`{"intent":"not_finance","amount":0,"currency":"","date":"","installments":0,"payment":"","description":"","shared":false}`)
		for _, msg := range []string{"escribime un poema", "contame un cuento", "qué hora es", "traducime esto", "sos un robot"} {
			if c := one(t, b.say(msg), "sendMessage"); !strings.HasPrefix(text(c), "Solo puedo registrar gastos") {
				t.Fatalf("%q: %q in %s", msg, text(c), usage())
			}
		}
		if c := one(t, b.say("contame un chiste largo"), "sendMessage"); !strings.Contains(text(c), "pausada en tu cuenta") {
			t.Errorf("paused text %q", text(c))
		}
		calls := b.send(map[string]any{"voice": map[string]any{"file_id": "voice-1", "duration": 5, "file_size": 20}})
		if c := one(t, calls, "sendMessage"); !strings.Contains(text(c), "pausada en tu cuenta") || len(calls) != 1 {
			t.Errorf("paused voice %+v", calls)
		}
		// The rules still read a typed amount.
		if c := one(t, b.say("pan 1200 efectivo"), "sendMessage"); !strings.Contains(text(c), "Pan · $ 1.200") {
			t.Errorf("rules while paused %q", text(c))
		}
		want := "parse_audio:telegram:ok,parse_receipt:telegram:ok," + strings.Repeat("parse_expense:telegram:off_topic,", 5) +
			"parse_expense:telegram:suspended"
		if got := usage(); got != want {
			t.Errorf("ai_usage\n got %s\nwant %s", got, want)
		}
	})
}
