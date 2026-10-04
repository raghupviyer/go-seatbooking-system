// Command burst reproduces an on-sale stampede against a running instance and
// checks the result. It only speaks HTTP, so it works against any deployment.
//
//	go run ./cmd/burst https://your-app.example.com
//
// Phases: (1) register and log in N users, (2) create two fresh shows,
// (3) hot-seat storm: every user reserves the same seat at the same instant,
// (4) stampede: every user fires a 1-4 seat reservation skewed to the front
// rows, with some requests also retried concurrently under the same
// idempotency key. It then prints the outcome distribution and reconciles what
// the server says against what the clients were told. Exit status is 1 if any
// invariant fails.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type config struct {
	base      string
	users     int
	rows      int
	cols      int
	limit     int
	setupConc int
	retryPct  int
	timeout   time.Duration
}

type result struct {
	user    int
	key     string
	seats   []string
	status  int // 0 = transport error
	outcome string
	latency time.Duration
}

func main() {
	var c config
	flag.IntVar(&c.users, "users", 300, "concurrent users")
	flag.IntVar(&c.rows, "rows", 10, "rows in the stampede show")
	flag.IntVar(&c.cols, "cols", 10, "seats per row in the stampede show")
	flag.IntVar(&c.limit, "limit", 4, "limit_per_user for the shows")
	flag.IntVar(&c.setupConc, "setup-concurrency", 16, "parallel register/login calls (bcrypt is slow)")
	flag.IntVar(&c.retryPct, "retry-pct", 10, "percent of stampede requests also sent a second time with the same key")
	flag.DurationVar(&c.timeout, "timeout", 60*time.Second, "per-request timeout")
	seed := flag.Int64("seed", time.Now().UnixNano(), "random seed")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: burst [flags] <BASE_URL>\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	c.base = strings.TrimRight(flag.Arg(0), "/")
	if c.base == "" {
		c.base = strings.TrimRight(os.Getenv("BASE_URL"), "/")
	}
	if c.base == "" || c.users < 2 || c.rows < 1 || c.cols < 1 || c.limit < 1 {
		flag.Usage()
		os.Exit(2)
	}

	rng := rand.New(rand.NewSource(*seed))
	cl := &client{
		base: c.base,
		hc: &http.Client{
			Timeout: c.timeout,
			Transport: &http.Transport{
				MaxIdleConns:        c.users * 2,
				MaxIdleConnsPerHost: c.users * 2,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
	runID := fmt.Sprintf("%x", time.Now().UnixNano()&0xffffffff)
	fmt.Printf("burst against %s  users=%d seed=%d run=%s\n", c.base, c.users, *seed, runID)

	if code, _, _, err := cl.do("GET", "/health/ready", "", nil); err != nil || code != 200 {
		fatalf("target is not ready (status %d, err %v)", code, err)
	}

	t0 := time.Now()
	tokens, err := setupUsers(cl, c, runID)
	if err != nil {
		fatalf("setup users: %v", err)
	}
	fmt.Printf("setup: %d users registered and logged in in %s\n", c.users, time.Since(t0).Round(time.Millisecond))

	// The hot show has a single seat so the storm is pure contention.
	hotShow, err := cl.createShow("burst-hot-"+runID, []string{"HOT1"}, c.limit)
	if err != nil {
		fatalf("create hot show: %v", err)
	}
	var grid []string
	for r := 0; r < c.rows; r++ {
		for k := 1; k <= c.cols; k++ {
			grid = append(grid, fmt.Sprintf("R%d-%d", r+1, k))
		}
	}
	mainShow, err := cl.createShow("burst-stampede-"+runID, grid, c.limit)
	if err != nil {
		fatalf("create stampede show: %v", err)
	}

	hotReqs := make([]request, c.users)
	for i := range hotReqs {
		hotReqs[i] = request{user: i, show: hotShow, seats: []string{"HOT1"}, key: fmt.Sprintf("hot-%s-%d", runID, i)}
	}
	hotRes, hotDur := fire(cl, tokens, hotReqs)
	report("hot-seat storm (same seat, all users)", hotRes, hotDur)

	var stampede []request
	for i := 0; i < c.users; i++ {
		n := 1 + rng.Intn(c.limit)
		r := request{user: i, show: mainShow, seats: pickSeats(rng, grid, n), key: fmt.Sprintf("st-%s-%d", runID, i)}
		stampede = append(stampede, r)
		if rng.Intn(100) < c.retryPct {
			stampede = append(stampede, r) // a double-click or client retry racing the original
		}
	}
	stRes, stDur := fire(cl, tokens, stampede)
	report("stampede (random seats, skewed to front rows)", stRes, stDur)

	fails := reconcile(cl, c, hotShow, hotRes, mainShow, stRes)
	fmt.Println()
	if len(fails) > 0 {
		fmt.Println("RESULT: FAIL")
		for _, f := range fails {
			fmt.Println("  -", f)
		}
		os.Exit(1)
	}
	fmt.Println("RESULT: PASS - no seat sold twice, counts reconcile, no 5xx")
}

// pickSeats returns n distinct seats, biased towards the start of the grid
// (the "good" seats everyone wants).
func pickSeats(rng *rand.Rand, grid []string, n int) []string {
	if n > len(grid) {
		n = len(grid)
	}
	seen := map[string]bool{}
	var out []string
	for len(out) < n {
		f := rng.Float64()
		s := grid[int(f*f*float64(len(grid)))]
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

type request struct {
	user  int
	show  string
	seats []string
	key   string
}

// fire sends every request at once, released together by a barrier.
func fire(cl *client, tokens []string, reqs []request) ([]result, time.Duration) {
	results := make([]result, len(reqs))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, rq := range reqs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = cl.reserve(tokens[rq.user], rq)
		}()
	}
	t0 := time.Now()
	close(start)
	wg.Wait()
	return results, time.Since(t0)
}

func setupUsers(cl *client, c config, runID string) ([]string, error) {
	tokens := make([]string, c.users)
	sem := make(chan struct{}, c.setupConc)
	var wg sync.WaitGroup
	var firstErr atomic.Value
	for i := 0; i < c.users; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			id := fmt.Sprintf("burst-%s-%d", runID, i)
			body := map[string]string{"user_id": id, "password": "burst-pass-123"}
			if code, b, _, err := cl.do("POST", "/user/register", "", body); err != nil || code != 200 {
				firstErr.CompareAndSwap(nil, fmt.Errorf("register %s: %d %s %v", id, code, b, err))
				return
			}
			code, b, _, err := cl.do("POST", "/user/login", "", body)
			var out struct {
				JWT string `json:"jwt_token"`
			}
			if err != nil || code != 200 || json.Unmarshal(b, &out) != nil || out.JWT == "" {
				firstErr.CompareAndSwap(nil, fmt.Errorf("login %s: %d %s %v", id, code, b, err))
				return
			}
			tokens[i] = out.JWT
		}()
	}
	wg.Wait()
	if e, ok := firstErr.Load().(error); ok {
		return nil, e
	}
	return tokens, nil
}

type client struct {
	hc   *http.Client
	base string
}

func (cl *client) do(method, path, token string, body any) (int, []byte, http.Header, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, cl.base+path, rd)
	if err != nil {
		return 0, nil, nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := cl.hc.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header, err
}

func (cl *client) createShow(name string, seats []string, limit int) (string, error) {
	code, b, _, err := cl.do("POST", "/shows", "", map[string]any{
		"name": name, "seats": seats, "price_paise": 25000, "limit_per_user": limit,
	})
	if err != nil || code != 201 {
		return "", fmt.Errorf("status %d: %s %v", code, b, err)
	}
	var out struct {
		ShowID string `json:"show_id"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return "", err
	}
	return out.ShowID, nil
}

func (cl *client) reserve(token string, rq request) result {
	start := time.Now()
	code, b, hdr, err := cl.do("POST", "/shows/"+rq.show+"/reserve", token,
		map[string]any{"seats": rq.seats, "idempotency_key": rq.key})
	r := result{user: rq.user, key: rq.key, seats: rq.seats, status: code, latency: time.Since(start)}
	if err != nil {
		r.status = 0
		r.outcome = "transport_error"
		return r
	}
	r.outcome = classify(code, b, hdr.Get("Idempotent-Replayed") == "true")
	return r
}

func classify(code int, body []byte, replayed bool) string {
	var e struct {
		Error       string   `json:"error"`
		Unavailable []string `json:"unavailable_seats"`
	}
	_ = json.Unmarshal(body, &e)
	msg := strings.ToLower(e.Error)
	switch {
	case code == 201 && replayed:
		return "idempotent_replay"
	case code == 201:
		return "confirmed"
	case code >= 500:
		return fmt.Sprintf("5xx (%d)", code)
	case code == 409 && len(e.Unavailable) > 0:
		return "declined: seat_taken"
	case code == 409 && strings.Contains(msg, "at most"):
		return "declined: per_user_limit"
	case code == 409 && strings.Contains(msg, "idempotency key"):
		return "declined: idempotency_conflict"
	case code == 409 && strings.Contains(msg, "retry"):
		return "declined: contention"
	case code == 400 && strings.Contains(msg, "unknown seats"):
		return "declined: unknown_seats"
	case code == 400:
		return "declined: invalid_request"
	case code == 401:
		return "declined: unauthorized"
	case code == 404:
		return "declined: show_not_found"
	case code == 429:
		return "declined: rate_limited"
	}
	return fmt.Sprintf("other (%d)", code)
}

func report(title string, rs []result, d time.Duration) {
	counts := map[string]int{}
	var lat []time.Duration
	for _, r := range rs {
		counts[r.outcome]++
		if r.status != 0 {
			lat = append(lat, r.latency)
		}
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pct := func(p float64) time.Duration {
		if len(lat) == 0 {
			return 0
		}
		return lat[int(p*float64(len(lat)-1))].Round(time.Millisecond)
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	fmt.Printf("\n== %s ==\n", title)
	fmt.Printf("%d requests in %s (%.0f req/s)   latency p50=%s p95=%s p99=%s max=%s\n",
		len(rs), d.Round(time.Millisecond), float64(len(rs))/d.Seconds(),
		pct(.5), pct(.95), pct(.99), pct(1))
	for _, k := range keys {
		fmt.Printf("  %-32s %6d  %5.1f%%\n", k, counts[k], 100*float64(counts[k])/float64(len(rs)))
	}
}

type showState struct {
	Counts struct {
		Total     int `json:"total_seats"`
		Available int `json:"available"`
		Held      int `json:"held"`
		Confirmed int `json:"confirmed"`
	} `json:"counts"`
	Seats []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"seats"`
}

// reconcile compares what clients were told with what the server holds and
// returns every violated invariant.
func reconcile(cl *client, c config, hotShow string, hot []result, mainShow string, st []result) []string {
	var fails []string
	fail := func(f string, a ...any) { fails = append(fails, fmt.Sprintf(f, a...)) }

	fmt.Println("\n== reconciliation ==")
	for _, set := range [][]result{hot, st} {
		for _, r := range set {
			if r.outcome == "transport_error" || strings.HasPrefix(r.outcome, "5xx") || strings.HasPrefix(r.outcome, "other") {
				fail("request %s (user %d) ended as %s", r.key, r.user, r.outcome)
			}
		}
	}

	check := func(name, showID string, rs []result) {
		code, b, _, err := cl.do("GET", "/shows/"+showID, "", nil)
		var s showState
		if err != nil || code != 200 || json.Unmarshal(b, &s) != nil {
			fail("%s: could not read show state (%d %v)", name, code, err)
			return
		}
		// Seats the clients were told they own: confirmed responses only
		// (a replay repeats an earlier confirmation).
		owner := map[string]string{}
		perUser := map[int]int{}
		confirmedReqs, soldSeats := 0, 0
		for _, r := range rs {
			if r.outcome != "confirmed" {
				continue
			}
			confirmedReqs++
			perUser[r.user] += len(r.seats)
			for _, seat := range r.seats {
				soldSeats++
				if prev, dup := owner[seat]; dup {
					fail("%s: seat %s confirmed twice (%s and %s)", name, seat, prev, r.key)
				}
				owner[seat] = r.key
			}
		}
		for u, n := range perUser {
			if n > c.limit {
				fail("%s: user %d holds %d seats, limit is %d", name, u, n, c.limit)
			}
		}
		status := map[string]string{}
		for _, seat := range s.Seats {
			status[seat.Name] = seat.Status
		}
		for seat := range owner {
			if status[seat] != "confirmed" {
				fail("%s: seat %s was confirmed to a client but the server shows %q", name, seat, status[seat])
			}
		}
		sum := s.Counts.Available + s.Counts.Held + s.Counts.Confirmed
		fmt.Printf("%-14s clients told: %d reservations = %d seats | server: confirmed=%d held=%d available=%d total=%d\n",
			name, confirmedReqs, soldSeats, s.Counts.Confirmed, s.Counts.Held, s.Counts.Available, s.Counts.Total)
		if soldSeats != s.Counts.Confirmed {
			fail("%s: clients were sold %d seats but server reports %d confirmed", name, soldSeats, s.Counts.Confirmed)
		}
		if sum != s.Counts.Total {
			fail("%s: available+held+confirmed (%d) != total (%d)", name, sum, s.Counts.Total)
		}
		if s.Counts.Held != 0 {
			fail("%s: %d seats still held after the burst", name, s.Counts.Held)
		}
		fmt.Printf("%-14s oversold=%v  available+held+confirmed==total: %v\n", name,
			soldSeats > s.Counts.Total, sum == s.Counts.Total)
	}
	check("hot-seat show", hotShow, hot)
	check("stampede show", mainShow, st)

	wins := 0
	for _, r := range hot {
		if r.outcome == "confirmed" {
			wins++
		}
	}
	if wins != 1 {
		fail("hot-seat storm: expected exactly 1 winner, got %d", wins)
	}
	return fails
}

func fatalf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "burst: "+f+"\n", a...)
	os.Exit(1)
}
