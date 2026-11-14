package ai_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LucianoR23/valuo_api/internal/ai"
)

// topic is a parse answer that can say "not an expense".
type topic struct {
	Intent string  `json:"intent"`
	Amount float64 `json:"amount"`
}

func (t topic) OffTopic() bool { return t.Intent == "not_finance" }

type alerted struct {
	mu    sync.Mutex
	calls []string
}

func (a *alerted) alert(_ context.Context, email, reason string, until time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, email+"|"+reason+"|"+until.Format(time.RFC3339))
}

func (a *alerted) list() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...)
}

func TestOffTopicSuspendsForADay(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.claude.replies = []reply{ok(`{"intent":"not_finance","amount":0}`, 100, 10)}
	now := time.Date(2026, 11, 14, 15, 0, 0, 0, time.UTC)
	var al alerted
	s := e.service(t, func(c *ai.Config) {
		c.Now = func() time.Time { return now }
		c.SuspendAlert = al.alert
	})
	ctx := context.Background()
	run := func(ch ai.Channel) error {
		req := e.request(e.verified, ai.TaskParseExpense)
		req.Channel = ch
		var got topic
		return s.Run(ctx, req, &got)
	}

	// Mail is not a person writing: it never counts.
	for range 5 {
		if err := run(ai.ChannelEmail); err != nil {
			t.Fatal(err)
		}
	}
	for i := range ai.SuspendOffTopic - 1 {
		if err := run(ai.ChannelTelegram); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if len(al.list()) != 0 {
		t.Fatalf("suspended too early: %v", al.list())
	}
	if err := run(ai.ChannelApp); err != nil {
		t.Fatalf("fifth off-topic answer is still returned: %v", err)
	}
	if got := al.list(); len(got) != 1 || !strings.Contains(got[0], "5 mensajes que no eran gastos") ||
		!strings.HasSuffix(got[0], "2026-11-15T15:00:00Z") {
		t.Fatalf("alerts %v", got)
	}

	calls := e.claude.count()
	if err := run(ai.ChannelApp); !errors.Is(err, ai.ErrSuspended) {
		t.Fatalf("suspended account: %v", err)
	}
	if e.claude.count() != calls {
		t.Error("a suspended account reached the provider")
	}
	rows := e.rows(t)
	if want := "ok,ok,ok,ok,ok,off_topic,off_topic,off_topic,off_topic,off_topic,suspended"; statuses(rows) != want {
		t.Errorf("statuses %s", statuses(rows))
	}

	// Another account is not affected; a day later it works again.
	var got topic
	if err := s.Run(ctx, e.request(addUser(t, e.pool, true), ai.TaskParseExpense), &got); err != nil {
		t.Errorf("other account: %v", err)
	}
	now = now.Add(ai.SuspendFor + time.Minute)
	e.claude.replies = []reply{ok(`{"intent":"expense","amount":2500}`, 100, 10)}
	if err := run(ai.ChannelApp); err != nil {
		t.Errorf("after the suspension: %v", err)
	}
	if len(al.list()) != 1 {
		t.Errorf("alerts %v", al.list())
	}
}

func TestCostFarAboveAverageSuspends(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	now := time.Date(2026, 11, 14, 15, 0, 0, 0, time.UTC)
	var al alerted
	s := e.service(t, func(c *ai.Config) {
		c.Now = func() time.Time { return now }
		c.SuspendAlert = al.alert
	})
	ctx := context.Background()
	// Three other accounts spent US$ 0.001 on a day this week; older days
	// do not count.
	for i := range 3 {
		u := addUser(t, e.pool, true)
		if _, err := e.pool.Exec(ctx, `INSERT INTO ai_usage (user_id, task, channel, status, cost_micro_usd, created_at)
			VALUES ($1, 'parse_expense', 'app', 'ok', 1000, $2), ($1, 'parse_expense', 'app', 'ok', 900000, $3)`,
			u, now.AddDate(0, 0, -i-1), now.AddDate(0, 0, -30)); err != nil {
			t.Fatal(err)
		}
	}

	// 100,000 input tokens of Haiku: US$ 0.01, not over the floor.
	e.claude.replies = []reply{ok(`{"intent":"expense","amount":1}`, 100_000, 0)}
	var got topic
	if err := s.Run(ctx, e.request(e.verified, ai.TaskParseExpense), &got); err != nil {
		t.Fatal(err)
	}
	if len(al.list()) != 0 {
		t.Fatalf("suspended at the floor: %v", al.list())
	}
	// Another US$ 0.01: 0.02 today, over 10 times 0.001.
	if err := s.Run(ctx, e.request(e.verified, ai.TaskParseExpense), &got); err != nil {
		t.Fatal(err)
	}
	if a := al.list(); len(a) != 1 || !strings.Contains(a[0], "US$ 0.02") || !strings.Contains(a[0], "US$ 0.00") {
		t.Fatalf("alerts %v", a)
	}
	if err := s.Run(ctx, e.request(e.verified, ai.TaskParseExpense), &got); !errors.Is(err, ai.ErrSuspended) {
		t.Errorf("after suspension: %v", err)
	}
}

func TestNoOtherAccountsNoCostRule(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	var al alerted
	s := e.service(t, func(c *ai.Config) { c.SuspendAlert = al.alert })
	e.claude.replies = []reply{ok(`{"intent":"expense","amount":1}`, 500_000, 0)} // US$ 0.05
	var got topic
	if err := s.Run(context.Background(), e.request(e.verified, ai.TaskParseExpense), &got); err != nil {
		t.Fatal(err)
	}
	var until *time.Time
	if err := e.pool.QueryRow(context.Background(), `SELECT ai_suspended_until FROM users WHERE id = $1`, e.verified).Scan(&until); err != nil {
		t.Fatal(err)
	}
	if until != nil || len(al.list()) != 0 {
		t.Errorf("suspended with no average: %v %v", until, al.list())
	}
}
