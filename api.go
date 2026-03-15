package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type OrderEntry struct {
	Price string
	Size  string
}

type OrderBook struct {
	Bids []OrderEntry
	Asks []OrderEntry
}

type MarketSide struct {
	Description string `json:"description"`
	Price       string `json:"price"`
	Long        bool   `json:"long"`
}

type Market struct {
	ID          string       `json:"id"`
	Question    string       `json:"question"`
	Title       string       `json:"title"`
	Slug        string       `json:"slug"`
	MarketSides []MarketSide `json:"marketSides"`
}

type Event struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Volume24h float64  `json:"volume24hr"`
	Liquidity float64  `json:"liquidity"`
	Markets   []Market `json:"markets"`
}

// bookPx / bookEntry parse the new API's order book format.
type bookPx struct {
	Value string `json:"value"`
}
type bookEntry struct {
	Px  bookPx `json:"px"`
	Qty string  `json:"qty"`
}

// signRequest builds an Ed25519 signature for the given method+path.
func signRequest(method, path string) (timestamp, sig string, err error) {
	secret := os.Getenv("PM_SECRET")
	if secret == "" {
		return "", "", fmt.Errorf("PM_SECRET not set")
	}
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	msg := ts + method + path
	keyBytes, err := base64.StdEncoding.DecodeString(secret)
	if err != nil {
		return "", "", fmt.Errorf("invalid PM_SECRET: %w", err)
	}
	if len(keyBytes) < 32 {
		return "", "", fmt.Errorf("PM_SECRET too short")
	}
	privKey := ed25519.NewKeyFromSeed(keyBytes[:32])
	sigBytes := ed25519.Sign(privKey, []byte(msg))
	return ts, base64.StdEncoding.EncodeToString(sigBytes), nil
}

func addAuthHeaders(req *http.Request, method, path string) error {
	ts, sig, err := signRequest(method, path)
	if err != nil {
		return err
	}
	req.Header.Set("X-PM-Access-Key", os.Getenv("PM_KEY_ID"))
	req.Header.Set("X-PM-Timestamp", ts)
	req.Header.Set("X-PM-Signature", sig)
	req.Header.Set("Accept", "application/json")
	return nil
}

// marketsToEvents wraps a flat market list into single-market Event wrappers
// so the existing UI model works unchanged.
func marketTitle(m Market) string {
	if m.Title != "" && m.Title != m.Question {
		return m.Question + ": " + m.Title
	}
	if m.Question != "" {
		return m.Question
	}
	return m.Title
}

func marketToEvent(m Market) Event {
	return Event{
		ID:      m.Slug,
		Title:   marketTitle(m),
		Markets: []Market{m},
	}
}

// marketsToEvents wraps a flat market list into single-market Event wrappers
// so the existing UI model works unchanged.
func marketsToEvents(markets []Market) []Event {
	events := make([]Event, len(markets))
	for i, m := range markets {
		events[i] = marketToEvent(m)
	}
	return events
}

func fetchEvent(slug string) (*Event, error) {
	resp, err := http.Get(GatewayURL + "/v1/market/slug/" + slug)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	// response is {"market": {...}}
	var wrapper struct {
		Market Market `json:"market"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, err
	}
	e := marketToEvent(wrapper.Market)
	return &e, nil
}

func fetchOrderBook(slug string) (*OrderBook, error) {
	resp, err := http.Get(GatewayURL + "/v1/markets/" + slug + "/book")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var raw struct {
		MarketData struct {
			Bids   []bookEntry `json:"bids"`
			Offers []bookEntry `json:"offers"`
		} `json:"marketData"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	ob := &OrderBook{}
	for _, e := range raw.MarketData.Bids {
		ob.Bids = append(ob.Bids, OrderEntry{Price: e.Px.Value, Size: e.Qty})
	}
	for _, e := range raw.MarketData.Offers {
		ob.Asks = append(ob.Asks, OrderEntry{Price: e.Px.Value, Size: e.Qty})
	}
	return ob, nil
}

// fetchOrderBooks fetches order books for all markets in an event concurrently.
// Returns a map of market slug → OrderBook.
func fetchOrderBooks(e *Event) map[string]OrderBook {
	type result struct {
		slug string
		ob   *OrderBook
	}
	var wg sync.WaitGroup
	ch := make(chan result)

	for _, mkt := range e.Markets {
		if mkt.Slug == "" {
			continue
		}
		wg.Add(1)
		go func(slug string) {
			defer wg.Done()
			ob, _ := fetchOrderBook(slug)
			ch <- result{slug, ob}
		}(mkt.Slug)
	}

	go func() {
		wg.Wait()
		close(ch)
	}()

	books := make(map[string]OrderBook)
	for r := range ch {
		if r.ob != nil {
			books[r.slug] = *r.ob
		}
	}
	return books
}

func fetchBalance() (string, error) {
	path := "/v1/account/balances"
	req, err := http.NewRequest("GET", AccountURL+path, nil)
	if err != nil {
		return "", err
	}
	if err := addAuthHeaders(req, "GET", path); err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result struct {
		Balances []struct {
			CurrentBalance float64 `json:"currentBalance"`
		} `json:"balances"`
	}
	if err := json.Unmarshal(body, &result); err != nil || len(result.Balances) == 0 {
		return strings.TrimSpace(string(body)), nil
	}
	return strconv.FormatFloat(result.Balances[0].CurrentBalance, 'f', 2, 64), nil
}

func fetchHotMarkets() ([]Event, error) {
	resp, err := http.Get(GatewayURL + "/v1/markets?active=true&closed=false&limit=20")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var result struct {
		Markets []Market `json:"markets"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return marketsToEvents(result.Markets), nil
}

func fetchPage(offset int) ([]Event, error) {
	url := GatewayURL + fmt.Sprintf("/v1/markets?active=true&closed=false&limit=500&offset=%d", offset)
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var result struct {
		Markets []Market `json:"markets"`
	}
	json.Unmarshal(body, &result)
	return marketsToEvents(result.Markets), nil
}

func searchEvents(query string) ([]Event, error) {
	const pageSize = 500

	var allEvents []Event
	var mu sync.Mutex

	for batchStart := 0; ; batchStart += 50 {
		type result struct {
			page   int
			events []Event
		}
		results := make([]result, 50)
		var wg sync.WaitGroup
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				offset := (batchStart + i) * pageSize
				events, _ := fetchPage(offset)
				results[i] = result{page: batchStart + i, events: events}
			}(i)
		}
		wg.Wait()

		anyNonEmpty := false
		for _, r := range results {
			if len(r.events) > 0 {
				anyNonEmpty = true
				mu.Lock()
				allEvents = append(allEvents, r.events...)
				mu.Unlock()
			}
		}
		if !anyNonEmpty {
			break
		}
	}

	lower := strings.ToLower(query)
	seen := make(map[string]bool)
	var matched []Event
	for _, e := range allEvents {
		if !seen[e.Title] && strings.Contains(strings.ToLower(e.Title), lower) {
			seen[e.Title] = true
			matched = append(matched, e)
		}
	}
	return matched, nil
}
