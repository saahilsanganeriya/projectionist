package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"projectionist/internal/llm"
	"projectionist/internal/llm/llmtest"
)

type memStore struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (s *memStore) GetJSON(_ context.Context, stage, key string, v any) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.m[stage+"/"+key]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(b, v)
}

func (s *memStore) PutJSON(_ context.Context, stage, key string, v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string][]byte{}
	}
	b, err := json.Marshal(v)
	s.m[stage+"/"+key] = b
	return err
}

// onClock is a client on g served in memory, with the default backoff, for a synctest bubble's fake clock.
func onClock(g *llmtest.Gateway, st llm.Store, tasks map[string]llm.Task) *llm.Client {
	c := llm.New(llm.Config{API: "http://gateway.test/v1", Tasks: tasks}, st, time.Now)
	c.Key = func() (string, error) { return "test-gateway-key", nil }
	c.HTTP = &http.Client{Transport: g.Transport()}
	return c
}

var ask = llm.Request{Messages: []llm.Message{llm.User(llm.Text("hi"))}, MaxTokens: 100, InputTokens: 1000}

// Catches: a 429 or 5xx failing the call instead of being retried; a server's Retry-After ignored (hammering a gateway
// that asked for a pause) or one of minutes waited out inside a run; retries past the fourth attempt; a 400 or a
// billing refusal retried (each a paid-for or pointless repeat), or a team's model restriction taken for billing; a timeout or a dropped answer not retried, or retried
// without the lost attempt counted as possibly billed. Waits are measured on the fake clock.
func TestFailurePolicy(t *testing.T) {
	ok := llmtest.Answer{Text: "fine", In: 10, Out: 5}
	cases := []struct {
		name        string
		answers     []llmtest.Answer
		calls       int
		minWait     time.Duration
		maxWait     time.Duration // exclusive
		maybeBilled int
		err         error // nil = answered
	}{
		{"429 with Retry-After", []llmtest.Answer{{Status: 429, Type: "rate_limit_exceeded", RetryAfter: "7"}, ok}, 2, 7 * time.Second, 7*time.Second + 1, 0, nil},
		{"5xx three times, jittered backoff", []llmtest.Answer{{Status: 503}, {Status: 500}, {Status: 502}, ok}, 4, 14 * time.Second, 28 * time.Second, 0, nil},
		{"5xx four times: gives up", []llmtest.Answer{{Status: 503}, {Status: 503}, {Status: 503}, {Status: 503}, ok}, 4, 14 * time.Second, 28 * time.Second, 0, &llm.APIError{}},
		{"Retry-After of 10 minutes: fails at once", []llmtest.Answer{{Status: 429, RetryAfter: "600"}, ok}, 1, 0, 1, 0, &llm.APIError{}},
		{"400: no retry", []llmtest.Answer{{Status: 400, Type: "invalid_request_error", Message: "bad image"}, ok}, 1, 0, 1, 0, &llm.APIError{}},
		{"403 no card: no retry", []llmtest.Answer{{Status: 403, Type: "customer_verification_required", Message: "requires a valid credit card on file"}, ok}, 1, 0, 1, 0, llm.ErrBilling},
		{"402 no credits: no retry", []llmtest.Answer{{Status: 402, Type: "insufficient_funds"}, ok}, 1, 0, 1, 0, llm.ErrBilling},
		{"403 free tier: no retry", []llmtest.Answer{{Status: 403, Type: "no_providers_available", // the live answer, 2026-10-06T00:03Z
			Message: "Free tier users do not have access to this model. Upgrade to paid credits at https://vercel.com/d?to=x for unrestricted access."}, ok},
			1, 0, 1, 0, llm.ErrBilling},
		{"403 model not allowed: no retry, not billing", []llmtest.Answer{{Status: 403, Type: "forbidden", Message: "model blocked by team restrictions"}, ok},
			1, 0, 1, 0, &llm.APIError{}},
		{"timeout, then an answer", []llmtest.Answer{{Hang: true}, ok}, 2, 122 * time.Second, 124 * time.Second, 1, nil},
		{"answer dropped, then an answer", []llmtest.Answer{{Drop: true}, ok}, 2, 2 * time.Second, 4 * time.Second, 1, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				g := llmtest.New(func(n int, _ llm.Request) llmtest.Answer { return c.answers[n-1] })
				s := onClock(g, &memStore{}, nil).Session("tonight")
				start := time.Now()
				res, err := s.Chat(context.Background(), ask)
				waited := time.Since(start)
				var apiErr *llm.APIError
				switch {
				case c.err == nil && (err != nil || res.Text != "fine"):
					t.Fatalf("got %q, %v; want the answer", res.Text, err)
				case c.err == llm.ErrBilling && !errors.Is(err, llm.ErrBilling):
					t.Fatalf("err %v; want a billing refusal", err)
				case c.err != nil && c.err != llm.ErrBilling && (!errors.As(err, &apiErr) || errors.Is(err, llm.ErrBilling)):
					t.Fatalf("err %v; want the gateway's error, not a billing refusal", err)
				}
				if n := len(g.Calls()); n != c.calls || res.Attempts != c.calls || waited < c.minWait || waited >= c.maxWait || res.MaybeBilled != c.maybeBilled {
					t.Fatalf("%d calls (%d attempts) over %v, %d maybe billed; want %d over [%v, %v), %d", n, res.Attempts, waited,
						res.MaybeBilled, c.calls, c.minWait, c.maxWait, c.maybeBilled)
				}
			})
		})
	}
}

// Catches: more than in_flight calls in the air at once (a backlog of 3,000 titles would open 3,000 connections to the
// gateway and hit its rate limit), or fewer (the bound serialising the work). 20 calls of 1 s each run as three waves.
func TestInFlightBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		inAir, most := 0, 0
		g := llmtest.New(func(int, llm.Request) llmtest.Answer {
			mu.Lock()
			inAir++
			most = max(most, inAir)
			mu.Unlock()
			time.Sleep(time.Second)
			mu.Lock()
			inAir--
			mu.Unlock()
			return llmtest.Answer{Text: "ok"}
		})
		s := onClock(g, &memStore{}, nil).Session("tonight")
		start := time.Now()
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() { s.Chat(context.Background(), ask) })
		}
		wg.Wait()
		if most != 8 || time.Since(start) != 3*time.Second {
			t.Fatalf("at most %d calls in the air, done in %v; want 8 and 3 s", most, time.Since(start))
		}
	})
}

// Catches: a call's cost under-counted against a budget: the gateway's own figure ignored, usage priced at a cheaper
// model than the fallback that answered, the cheaper base price used where a region lists a dearer one, a lost
// attempt counted as free; the worst case (the reservation) priced below the dearest model the task may reach, or below
// what a provider bills at its peak (twice the model list's price, live); and a
// cost figure in a shape the client does not know (an object, text with units: the documented answer has none) taken
// for an unreadable answer, asked again and paid twice.
func TestCost(t *testing.T) {
	cheap, dear := llmtest.Model{ID: "test/cheap", In: 1, Out: 2}, llmtest.Model{ID: "test/dear", In: 10, Out: 20}
	tasks := map[string]llm.Task{"t": {Model: "test/cheap", Fallbacks: []string{"test/dear"}}}
	cases := []struct {
		name    string
		answers []llmtest.Answer
		usd     float64
	}{
		{"the gateway's figure", []llmtest.Answer{{Text: "x", In: 1000, Out: 100, Meta: `{"gateway":{"cost":"$0.0123"}}`}}, 0.0123},
		{"usage at the model's price", []llmtest.Answer{{Text: "x", In: 1000, Out: 100}}, (1000*1 + 100*2) / 1e6},
		{"a cost in another shape", []llmtest.Answer{{Text: "x", In: 1000, Out: 100, Meta: `{"gateway":{"cost":{"usd":"9"}}}`}}, (1000*1 + 100*2) / 1e6},
		{"a cost with units", []llmtest.Answer{{Text: "x", In: 1000, Out: 100, Meta: `{"gateway":{"cost":"9 USD"}}`}}, (1000*1 + 100*2) / 1e6},
		{"metadata not an object", []llmtest.Answer{{Text: "x", In: 1000, Out: 100, Meta: `"routed"`}}, (1000*1 + 100*2) / 1e6},
		{"a fallback answered", []llmtest.Answer{{Text: "x", In: 1000, Out: 100, Model: "test/dear"}}, (1000*10 + 100*20) / 1e6},
		{"a lost attempt at the worst case", []llmtest.Answer{{Drop: true}, {Text: "x", In: 1000, Out: 100, Meta: `{"gateway":{"cost":0.001}}`}}, 0.001 + (1000*10+100*20)/1e6},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := llmtest.New(func(n int, _ llm.Request) llmtest.Answer { return c.answers[n-1] })
			g.Models = []llmtest.Model{cheap, dear}
			s := llmtest.Client(t, g, &memStore{}, time.Now, llm.Config{Tasks: tasks}).Session("t")
			worst, err := s.WorstCase(context.Background(), llm.Request{InputTokens: 1000, MaxTokens: 100})
			if err != nil || math.Abs(worst-0.012) > 1e-12 {
				t.Fatalf("worst case $%v, %v; want $0.012 (the dear fallback's price)", worst, err)
			}
			res, err := s.Chat(context.Background(), ask)
			if err != nil || math.Abs(res.CostUSD-c.usd) > 1e-12 || len(g.Calls()) != len(c.answers) {
				t.Fatalf("cost $%v, %v, %d calls; want $%v in %d", res.CostUSD, err, len(g.Calls()), c.usd, len(c.answers))
			}
			if got := g.Calls()[0].Models; len(got) != 1 || got[0] != "test/dear" {
				t.Fatalf("fallbacks sent %v", got)
			}
		})
	}
	t.Run("the dearest price billed", func(t *testing.T) {
		// The real entries' shapes: in the model list the US region charges more than the base price; among the
		// providers, alibaba doubles its price in its peak window (the price a live call was billed, 2026-10-06T01:12Z).
		list := `{"data":[{"id":"deepseek/deepseek-v4-flash-0731","type":"language","pricing":{"input":"0.000000076","output":"0.000000153",
			"input_cache_read":"0.000000014","varies_by_provider":true,"regional":{"us":{"input":"0.00000013","output":"0.00000026"}},
			"output_tiers":[{"cost":"$0.0000003","min":0}]}}]}`
		providers := `{"data":{"id":"deepseek/deepseek-v4-flash-0731","endpoints":[
			{"provider_name":"alibaba","pricing":{"prompt":"0.00000005","completion":"0.0000002","request":"0",
				"peak_pricing":{"multiplier":2,"windows":[{"start_minute_utc":0,"end_minute_utc":840}]}}},
			{"provider_name":"runware","pricing":{"prompt":"0.000000076","completion":"0.000000153","request":"0"}}]}}`
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/endpoints") {
				w.Write([]byte(providers))
				return
			}
			w.Write([]byte(list))
		}))
		defer srv.Close()
		c := llm.New(llm.Config{API: srv.URL + "/v1"}, &memStore{}, time.Now)
		c.Key = func() (string, error) { return "k", nil }
		m, err := c.Model(context.Background(), "deepseek/deepseek-v4-flash-0731")
		if err != nil || m.In != 0.00000013 || math.Abs(m.Out-0.0000004) > 1e-18 {
			t.Fatalf("%+v, %v; want the US input price and alibaba's output price at its peak", m, err)
		}
	})
}

// Catches: a broken model or key paid for and logged thousands of times in one run instead of parking after five
// failed calls in a row; a success not resetting the count (three bad hours parking a healthy day); a billing refusal
// counted as a model failure, retried call after call, parked as if the hold had kept it unsent (the alert would say
// "not sent" of a call the gateway refused), or forgotten by the next run's alert; and the refusal kept after the
// gateway answers again.
func TestSessionParks(t *testing.T) {
	var mu sync.Mutex
	fail, billing := true, false
	g := llmtest.New(func(int, llm.Request) llmtest.Answer {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case billing:
			return llmtest.Answer{Status: 403, Type: "customer_verification_required", Message: "requires a valid credit card on file"}
		case fail:
			return llmtest.Answer{Status: 400, Type: "invalid_request_error"}
		}
		return llmtest.Answer{Text: "ok"}
	})
	st := &memStore{}
	clock := time.Date(2026, 10, 5, 23, 0, 0, 0, time.UTC)
	c := llmtest.Client(t, g, st, func() time.Time { return clock }, llm.Config{})
	ctx := context.Background()
	set := func(f, b bool) { mu.Lock(); fail, billing = f, b; mu.Unlock() }

	s := c.Session("tonight")
	for range 4 {
		s.Chat(ctx, ask)
	}
	set(false, false)
	s.Chat(ctx, ask) // resets the count
	set(true, false)
	for range 4 {
		s.Chat(ctx, ask)
	}
	if s.Parked() != nil {
		t.Fatalf("parked after 4 failures in a row: %v", s.Parked())
	}
	s.Chat(ctx, ask)
	_, err := s.Chat(ctx, ask)
	if !errors.Is(err, llm.ErrParked) || !errors.Is(s.Parked(), llm.ErrParked) || len(g.Calls()) != 10 {
		t.Fatalf("after 5 failures in a row: %v, %d calls; want parked at the fifth and the sixth not sent", err, len(g.Calls()))
	}

	set(false, true)
	s = c.Session("tonight")
	_, err = s.Chat(ctx, ask)
	_, again := s.Chat(ctx, ask)
	if !errors.Is(err, llm.ErrBilling) || !errors.Is(again, llm.ErrParked) || len(g.Calls()) != 11 ||
		!strings.Contains(s.Parked().Error(), "since 2026-10-05T23:00:00Z") || !errors.Is(s.Parked(), llm.ErrBilling) ||
		strings.Contains(s.Parked().Error(), "not sent") {
		t.Fatalf("billing: %v then %v after %d calls, parked %v; want one call, parked on the gateway's refusal since then", err, again,
			len(g.Calls()), s.Parked())
	}
	clock = clock.Add(6 * time.Hour) // the next run: a new client, as after a restart, still refused
	c = llmtest.Client(t, g, st, func() time.Time { return clock }, llm.Config{})
	s = c.Session("tonight")
	s.Chat(ctx, ask)
	if !strings.Contains(s.Parked().Error(), "since 2026-10-05T23:00:00Z") {
		t.Fatalf("the next run's refusal: %v; want it dated from the first", s.Parked())
	}
	set(false, false)
	clock = clock.Add(time.Hour) // the next try (TestBillingHold owns the hold)
	s = c.Session("tonight")
	if _, err := s.Chat(ctx, ask); err != nil {
		t.Fatal(err)
	}
	set(false, true)
	s = c.Session("tonight")
	s.Chat(ctx, ask)
	if !strings.Contains(s.Parked().Error(), "since 2026-10-06T06:00:00Z") {
		t.Fatalf("a refusal after an answered call: %v; want it dated anew", s.Parked())
	}
}

// Catches: a gateway that refuses for billing asked by every run of every stage (tonight runs every 30 min: 48 refused
// calls and 48 alerts a day) instead of once an hour across the process and its restarts; the hourly try sent by every
// caller at once; the callers queued behind a try that was answered held back for the hour; those behind a refused try
// moving the next try or burying the gateway's answer under their own "not sent"; and the one alert raised again by a
// later run, a restart or another stage, or not raised for a new refusal after the gateway answered. On the fake clock,
// with the try's answer held until the other callers queue behind it.
func TestBillingHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		refuse, gate := true, chan struct{}(nil)
		g := llmtest.New(func(int, llm.Request) llmtest.Answer {
			mu.Lock()
			r, wait := refuse, gate
			mu.Unlock()
			if wait != nil {
				<-wait
			}
			if r {
				return llmtest.Answer{Status: 403, Type: "customer_verification_required", Message: "requires a valid credit card on file"}
			}
			return llmtest.Answer{Text: "ok"}
		})
		set := func(r bool, w chan struct{}) { mu.Lock(); refuse, gate = r, w; mu.Unlock() }
		st := &memStore{}
		ctx := context.Background()
		// burst sends 8 calls, each on its own session (another task or stage); the first to reach the gateway gets
		// its answer only once the other 7 are queued.
		burst := func(c *llm.Client) []error {
			wait := make(chan struct{})
			set(refuse, wait)
			errs := make([]error, 8)
			var wg sync.WaitGroup
			for i := range errs {
				wg.Go(func() { _, errs[i] = c.Session("posters").Chat(ctx, ask) })
			}
			synctest.Wait()
			close(wait)
			wg.Wait()
			set(refuse, nil)
			return errs
		}

		start := time.Now()
		c := onClock(g, st, nil)
		_, err := c.Session("tonight").Chat(ctx, ask)
		if told, again := c.TellBilling(ctx), c.TellBilling(ctx); !errors.Is(err, llm.ErrBilling) || !told || again || len(g.Calls()) != 1 {
			t.Fatalf("the first refusal: %v, %d calls, told %v then %v; want one call and one alert", err, len(g.Calls()), told, again)
		}
		time.Sleep(30 * time.Minute) // the next tonight run, after a restart
		c = onClock(g, st, nil)
		s := c.Session("posters")
		parked := s.Parked()
		_, err = s.Chat(ctx, ask)
		if !errors.Is(parked, llm.ErrBilling) || !errors.Is(err, llm.ErrBilling) || len(g.Calls()) != 1 || c.TellBilling(ctx) ||
			!strings.Contains(parked.Error(), "since "+start.Format(time.RFC3339)) ||
			!strings.Contains(parked.Error(), "tried again from "+start.Add(time.Hour).Format(time.RFC3339)) {
			t.Fatalf("30 min later: parked %v, %v, %d calls; want parked before any call, none sent, no second alert", parked, err, len(g.Calls()))
		}
		time.Sleep(31 * time.Minute) // past the hour since the refusal
		for i, err := range burst(c) {
			if !errors.Is(err, llm.ErrBilling) {
				t.Fatalf("call %d of the hourly burst: %v; want a billing refusal", i, err)
			}
		}
		parked = c.Session("tonight").Parked()
		if len(g.Calls()) != 2 || c.TellBilling(ctx) || strings.Count(fmt.Sprint(parked), "its last answer") != 1 ||
			!strings.Contains(fmt.Sprint(parked), "tried again from "+time.Now().Add(time.Hour).Format(time.RFC3339)) {
			t.Fatalf("the hourly try: %d calls in all, then %v; want 2 (one try for 8 callers), no second alert, the next try an "+
				"hour after it and the gateway's own answer", len(g.Calls()), parked)
		}
		time.Sleep(61 * time.Minute)
		set(false, nil)
		for i, err := range burst(c) {
			if err != nil {
				t.Fatalf("call %d behind an answered try: %v", i, err)
			}
		}
		if len(g.Calls()) != 10 {
			t.Fatalf("%d calls in all; want 10: the try and the 7 behind it", len(g.Calls()))
		}
		set(true, nil)
		c.Session("tonight").Chat(ctx, ask)
		if len(g.Calls()) != 11 || !c.TellBilling(ctx) {
			t.Fatalf("a new refusal after an answered call: %d calls; want it sent and told", len(g.Calls()))
		}
	})
}

// Catches: a judge that reasons (paid output, and minutes per call) because "off" was sent in a form the model does
// not take, or an effort sent to a model that lists none; and fallbacks, the bearer key or a one-part message sent
// in a shape the gateway does not read.
func TestRequestShape(t *testing.T) {
	cases := []struct {
		model  llmtest.Model
		effort string
		want   string
	}{
		{llmtest.Model{ID: "m/none", Efforts: []string{"none", "low"}, Toggle: true}, "off", `{"effort":"none"}`},
		{llmtest.Model{ID: "m/toggle", Toggle: true}, "off", `{"enabled":false}`},
		{llmtest.Model{ID: "m/least", Efforts: []string{"low", "high", "max"}}, "off", `{"effort":"low"}`},
		{llmtest.Model{ID: "m/silent"}, "off", `null`},
		{llmtest.Model{ID: "m/least", Efforts: []string{"low", "high"}}, "high", `{"effort":"high"}`},
	}
	for _, c := range cases {
		g := llmtest.New(nil)
		g.Models = []llmtest.Model{c.model}
		s := llmtest.Client(t, g, &memStore{}, time.Now, llm.Config{Tasks: map[string]llm.Task{"t": {Model: c.model.ID, Reasoning: c.effort}}}).Session("t")
		if _, err := s.Chat(context.Background(), llm.Request{Messages: []llm.Message{llm.System("be brief"), llm.User(llm.Text("a"), llm.JPEG([]byte{1}))}}); err != nil {
			t.Fatal(err)
		}
		got, _ := json.Marshal(g.Calls()[0].Reasoning)
		if string(got) != c.want {
			t.Errorf("%s %q: reasoning %s; want %s", c.model.ID, c.effort, got, c.want)
		}
		if k := g.Keys()[0]; k != "Bearer test-SECRET-gateway-key" {
			t.Errorf("Authorization %q", k)
		}
	}
	one, _ := json.Marshal(llm.System("be brief"))
	two, _ := json.Marshal(llm.User(llm.Text("a"), llm.JPEG([]byte{1})))
	if string(one) != `{"role":"system","content":"be brief"}` ||
		string(two) != `{"role":"user","content":[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,AQ=="}}]}` {
		t.Fatalf("messages: %s %s", one, two)
	}
}

// Catches: the catalogue fetched on every call (409 models, each run) or never again (a price change missed for
// good); a gateway outage failing calls that a day-old copy can price; and a model the copy lacks refetched on every
// call.
func TestCatalogueCachedDaily(t *testing.T) {
	g := llmtest.New(nil)
	clock := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	c := llmtest.Client(t, g, &memStore{}, func() time.Time { return clock }, llm.Config{})
	ctx := context.Background()
	steps := []struct {
		after   time.Duration
		id      string
		down    bool
		listed  int
		wantErr bool
	}{
		{0, "deepseek/deepseek-v4-flash-0731", false, 1, false},
		{23*time.Hour + 30*time.Minute, "deepseek/deepseek-v4-flash-0731", false, 1, false},
		{time.Hour, "deepseek/deepseek-v4-flash-0731", false, 2, false},
		{10 * time.Minute, "nobody/unknown", false, 2, true},                // a copy 10 minutes old: no refetch for an unknown id
		{55 * time.Minute, "nobody/unknown", false, 3, true},                // over an hour old: one refetch
		{25 * time.Hour, "deepseek/deepseek-v4-flash-0731", true, 3, false}, // the gateway is down: the old copy stands
	}
	for i, s := range steps {
		clock = clock.Add(s.after)
		if s.down {
			c.HTTP.Transport = failing{}
		}
		_, err := c.Model(ctx, s.id)
		if (err != nil) != s.wantErr || g.Listed() != s.listed {
			t.Fatalf("step %d: err %v, listed %d times; want err %v, %d", i, err, g.Listed(), s.wantErr, s.listed)
		}
	}
}

type failing struct{}

func (failing) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection refused")
}

// Catches a task table that sets only some keys (its fallbacks, say) sending an empty model, or the tonight task
// losing its reasoning or its fallback, because the table replaced the default wholesale.
func TestPartialTaskKeepsItsDefaults(t *testing.T) {
	c := llm.New(llm.Config{Tasks: map[string]llm.Task{"posters": {Fallbacks: []string{"google/gemma-4-31b-it"}}, "tonight": {Model: "m/x"}}}, nil, time.Now)
	if p, n := c.Task("posters"), c.Task("tonight"); p.Model != llm.VisionModel || p.Reasoning != "none" || len(p.Fallbacks) != 1 ||
		n.Model != "m/x" || n.Reasoning != "off" || !slices.Equal(n.Fallbacks, []string{llm.FallbackModel}) ||
		c.Task("collections").Model != llm.VisionModel {
		t.Fatalf("posters %+v, tonight %+v, collections %+v", p, n, c.Task("collections"))
	}
}
