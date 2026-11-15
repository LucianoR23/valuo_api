package telegram

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	sqlcgen "github.com/LucianoR23/valuo_api/internal/db/sqlc"
	"github.com/LucianoR23/valuo_api/internal/domain"
	"github.com/LucianoR23/valuo_api/internal/expenses"
)

func TestStartCode(t *testing.T) {
	cases := map[string]struct {
		code string
		ok   bool
	}{
		"/start abc":          {"abc", true},
		"/start@test_bot abc": {"abc", true},
		"/start":              {"", false},
		"/startabc":           {"", false},
		"café 2500":           {"", false},
	}
	for in, want := range cases {
		code, ok := startCode(in)
		if code != want.code || ok != want.ok {
			t.Errorf("%q: %q %v", in, code, ok)
		}
	}
}

func TestSummaryAndDays(t *testing.T) {
	ar, _ := time.LoadLocation("America/Argentina/Buenos_Aires")
	// 00:30 in Buenos Aires is still the previous day in UTC terms of the
	// server; "hoy" must follow the app's zone.
	now := time.Date(2026, 11, 15, 3, 30, 0, 0, time.UTC)
	s := NewService(Deps{}, Config{Location: ar, Now: func() time.Time { return now }})
	d := draft{Description: "Nafta", Amount: "30000", Currency: "ARS", SpentAt: "2026-11-15",
		Installments: 3, Method: "Visa Galicia", Category: "Auto", Shared: true}
	want := "Nafta · $ 30.000\nVisa Galicia · 3 cuotas · hoy · Auto · Hogar"
	if got := s.summary1(d); got != want {
		t.Errorf("summary %q", got)
	}
	for date, want := range map[string]string{"2026-11-14": "ayer", "2026-10-02": "2/10", "2025-12-31": "31/12/2025"} {
		if got := s.day(date); got != want {
			t.Errorf("%s: %q", date, got)
		}
	}
	usd := draft{Description: "Libro", Amount: "12.5", Currency: "USD", SpentAt: "2026-11-15", Installments: 1}
	if got := s.summary1(usd); got != "Libro · US$ 12,50\nhoy" {
		t.Errorf("usd %q", got)
	}
}

func TestCommittedText(t *testing.T) {
	card := domain.CardCommitted{}
	if got := committedText(domain.Committed{}); got != "No tenés tarjetas de crédito cargadas." {
		t.Errorf("no cards %q", got)
	}
	c := domain.Committed{Cards: []domain.CardCommitted{card}, Months: []domain.PeriodTotals{
		{PeriodYM: "2026-12", Totals: []domain.CurrencyAmount{
			{Currency: "ARS", Amount: decimal.RequireFromString("120000")},
			{Currency: "USD", Amount: decimal.RequireFromString("50")},
		}},
		{PeriodYM: "2027-01", Totals: []domain.CurrencyAmount{{Currency: "ARS", Amount: decimal.Zero}}},
		{PeriodYM: "2027-02", Totals: []domain.CurrencyAmount{{Currency: "ARS", Amount: decimal.RequireFromString("8000.5")}}},
	}}
	want := "Comprometido en tarjetas:\nDiciembre 2026: $ 120.000 · US$ 50\nFebrero 2027: $ 8.000,50"
	if got := committedText(c); got != want {
		t.Errorf("committed %q", got)
	}
}

func TestMethodOptions(t *testing.T) {
	ids := make([]uuid.UUID, 8)
	methods := make([]sqlcgen.TelegramMethodsRow, 8)
	for i := range ids {
		ids[i] = uuid.New()
		methods[i] = sqlcgen.TelegramMethodsRow{ID: ids[i], Name: string(rune('A' + i))}
	}
	got := methodOptions(methods, []uuid.UUID{ids[7], ids[5]})
	names := []string{}
	for _, o := range got {
		names = append(names, o.Name)
	}
	if strings.Join(names, "") != "HFABCD" {
		t.Errorf("options %v", names)
	}
}

func TestCardButtonsFitTelegram(t *testing.T) {
	s := NewService(Deps{}, Config{})
	opts := make([]option, MaxMethodButtons)
	for i := range opts {
		opts[i] = option{ID: uuid.New(), Name: "M"}
	}
	_, kb := s.card(uuid.New(), draft{Amount: "1", Currency: "ARS", SpentAt: "2026-11-14", Options: opts})
	n := 0
	for _, row := range kb {
		for _, b := range row {
			n++
			if len(b.CallbackData) > 64 {
				t.Errorf("callback data %d bytes: %s", len(b.CallbackData), b.CallbackData)
			}
		}
	}
	if n != MaxMethodButtons+2 {
		t.Errorf("buttons %d", n)
	}
}

func TestChatLimiter(t *testing.T) {
	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	l := newChatLimiter(func() time.Time { return now })
	for i := 0; i < chatPerMinute; i++ {
		if !l.allow(1, false) {
			t.Fatalf("update %d refused", i)
		}
	}
	if l.allow(1, false) {
		t.Error("over the limit")
	}
	if !l.allow(2, false) {
		t.Error("other chat refused")
	}
	now = now.Add(time.Minute)
	if !l.allow(1, false) {
		t.Error("a minute later refused")
	}

	if !l.allow(3, true) || l.allow(3, true) {
		t.Error("unlinked reply not once")
	}
	now = now.Add(unlinkedRepeat)
	if !l.allow(3, true) {
		t.Error("unlinked reply never again")
	}
}

func TestRecentIDs(t *testing.T) {
	r := newRecentIDs(2)
	if !r.add(1) || r.add(1) || !r.add(2) || !r.add(3) {
		t.Fatal("dedupe")
	}
	if !r.add(1) {
		t.Error("oldest id not forgotten")
	}
}

func TestPickFile(t *testing.T) {
	f, ok := pickFile(&Message{Voice: &Voice{FileID: "v", Duration: 30, FileSize: 100}})
	if !ok || !f.audio || f.tooBig || f.mimeType != "audio/ogg" || f.id != "v" {
		t.Errorf("voice %+v", f)
	}
	if f, _ := pickFile(&Message{Voice: &Voice{FileID: "v", Duration: 31}}); !f.tooBig || f.tooBigText != textVoiceTooLong {
		t.Errorf("long voice %+v", f)
	}
	if _, ok := pickFile(&Message{Audio: &Voice{FileID: "a", MimeType: "audio/flac"}}); ok {
		t.Error("flac accepted")
	}
	if f, ok := pickFile(&Message{Photo: []PhotoSize{{FileID: "x", Width: 9, Height: 9, FileSize: 6 << 20}}}); !ok || !f.tooBig {
		t.Errorf("huge photo %+v", f)
	}
	f, ok = pickFile(&Message{Document: &Document{FileID: "d", MimeType: "image/png", FileSize: 10}})
	if !ok || f.audio || f.mimeType != "image/png" || f.tooBig {
		t.Errorf("png file %+v", f)
	}
	if _, ok := pickFile(&Message{Document: &Document{FileID: "d", MimeType: "application/pdf"}}); ok {
		t.Error("pdf accepted")
	}
	if heardLine(draft{}) != "" || heardLine(draft{Heard: "Leí: «x»"}) != "Leí: «x»\n\n" {
		t.Error("heard line")
	}
}

func TestSimilarText(t *testing.T) {
	ar, _ := time.LoadLocation("America/Argentina/Buenos_Aires")
	now := time.Date(2026, 11, 15, 15, 0, 0, 0, time.UTC)
	s := NewService(Deps{}, Config{Location: ar, Now: func() time.Time { return now }})
	me, ana := uuid.New(), uuid.New()
	dup := expenses.Duplicate{Description: "Carrefour", Amount: decimal.NewFromInt(12500), Currency: "ARS",
		SpentAt: time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC), Status: "confirmed", CreatedBy: ana, CreatedByName: "Ana"}
	if got, want := s.similarText(me, dup), "⚠️ Parecido a uno que ya está: Carrefour · $ 12.500 · ayer (Ana)"; got != want {
		t.Errorf("another member's: %q, want %q", got, want)
	}
	if got := s.similarText(ana, dup); strings.Contains(got, "(Ana)") {
		t.Errorf("own expense names its author: %q", got)
	}
	dup.Status, dup.Description, dup.SpentAt = "draft", "Luz", time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	if got, want := s.similarText(me, dup), "⚠️ Tenés pendiente Luz del 10/11. Si es esa factura, completala desde la app."; got != want {
		t.Errorf("pending bill: %q, want %q", got, want)
	}
	if similarLine(draft{}) != "" || similarLine(draft{Similar: "x"}) != "\n\nx" {
		t.Error("similarLine")
	}
}
