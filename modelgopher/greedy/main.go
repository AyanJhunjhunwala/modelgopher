// Greedy — order-book sniper for Polymarket US.
// Watches every book frame for asks priced dramatically below the best bid
// (mispriced / fat-finger orders) and immediately buys + sells for profit.
//
// Usage: ./greedy_bin <market-slug> [flags]
//
//	./greedy_bin aec-nba-den-lal-2026-03-14
//	./greedy_bin aec-nba-den-lal-2026-03-14 --discount 0.05 --budget 50
package main

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/gorilla/websocket"
)

// ── Config ────────────────────────────────────────────────────────────────────

const (
	accountURL = "https://api.polymarket.us"
	wsURL      = "wss://api.polymarket.us/v1/ws/markets"
)

var (
	slug       string
	budget     float64
	discount   float64
	maxPerTrade float64
)

// ── Auth ──────────────────────────────────────────────────────────────────────

var (
	privKey ed25519.PrivateKey
	keyID   string
)

func initAuth() {
	raw, err := base64.StdEncoding.DecodeString(os.Getenv("PM_SECRET"))
	if err != nil || len(raw) < 32 {
		log.Fatal("bad PM_SECRET — check your .env")
	}
	privKey = ed25519.NewKeyFromSeed(raw[:32])
	keyID = os.Getenv("PM_KEY_ID")
	if keyID == "" {
		log.Fatal("PM_KEY_ID not set")
	}
}

func sign(method, path string) (ts, sig string) {
	t := strconv.FormatInt(time.Now().UnixMilli(), 10)
	s := ed25519.Sign(privKey, []byte(t+method+path))
	return t, base64.StdEncoding.EncodeToString(s)
}

func setAuth(req *http.Request, method, path string) {
	ts, sig := sign(method, path)
	req.Header.Set("X-PM-Access-Key", keyID)
	req.Header.Set("X-PM-Timestamp", ts)
	req.Header.Set("X-PM-Signature", sig)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
}

// ── HTTP client ───────────────────────────────────────────────────────────────

var client = &http.Client{
	Transport: &http.Transport{
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	},
	Timeout: 5 * time.Second,
}

func apiPost(path string, body any) ([]byte, int, error) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", accountURL+path, bytes.NewReader(b))
	setAuth(req, "POST", path)
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return raw, resp.StatusCode, nil
}

// ── Orders ────────────────────────────────────────────────────────────────────

func placeOrder(intent string, price float64, qty int) (string, error) {
	body := map[string]any{
		"marketSlug":           slug,
		"type":                 "ORDER_TYPE_LIMIT",
		"price":                map[string]any{"value": strconv.FormatFloat(price, 'f', 4, 64), "currency": "USD"},
		"quantity":             qty,
		"tif":                  "TIME_IN_FORCE_GOOD_TILL_CANCEL",
		"intent":               intent,
		"manualOrderIndicator": "MANUAL_ORDER_INDICATOR_MANUAL",
	}
	raw, status, err := apiPost("/v1/orders", body)
	if err != nil {
		return "", err
	}
	var result struct {
		ID string `json:"id"`
	}
	json.Unmarshal(raw, &result)
	if status != 200 || result.ID == "" {
		return "", fmt.Errorf("rejected %d: %s", status, raw)
	}
	return result.ID, nil
}

// ── Book parsing ──────────────────────────────────────────────────────────────

type level struct{ price, qty float64 }
type snap struct{ bids, asks []level }

var bidBuf [64]level
var askBuf [64]level

func parseBook(data []byte) (snap, bool) {
	var frame struct {
		MarketData struct {
			MarketSlug string `json:"marketSlug"`
			Bids       []struct {
				Px  struct{ Value string `json:"value"` } `json:"px"`
				Qty string                                `json:"qty"`
			} `json:"bids"`
			Offers []struct {
				Px  struct{ Value string `json:"value"` } `json:"px"`
				Qty string                                `json:"qty"`
			} `json:"offers"`
		} `json:"marketData"`
	}
	if err := json.Unmarshal(data, &frame); err != nil || frame.MarketData.MarketSlug != slug {
		return snap{}, false
	}
	b := bidBuf[:0]
	for _, e := range frame.MarketData.Bids {
		p, _ := strconv.ParseFloat(e.Px.Value, 64)
		q, _ := strconv.ParseFloat(e.Qty, 64)
		b = append(b, level{p, q})
	}
	a := askBuf[:0]
	for _, e := range frame.MarketData.Offers {
		p, _ := strconv.ParseFloat(e.Px.Value, 64)
		q, _ := strconv.ParseFloat(e.Qty, 64)
		a = append(a, level{p, q})
	}
	return snap{b, a}, true
}

// ── Budget tracker — lock-free atomic float64 ─────────────────────────────────

var spentBits uint64

func loadSpent() float64 {
	b := atomic.LoadUint64(&spentBits)
	return *(*float64)(unsafe.Pointer(&b))
}

func casSpent(old, new float64) bool {
	ob := *(*uint64)(unsafe.Pointer(&old))
	nb := *(*uint64)(unsafe.Pointer(&new))
	return atomic.CompareAndSwapUint64(&spentBits, ob, nb)
}

func addSpent(delta float64) {
	for {
		cur := loadSpent()
		if casSpent(cur, cur+delta) {
			return
		}
	}
}

func fmin(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// ── Open order types ──────────────────────────────────────────────────────────

type nestedPrice struct {
	Value string `json:"value"`
}

type openOrder struct {
	ID       string      `json:"id"`
	Intent   string      `json:"intent"`
	Price    nestedPrice `json:"price"`
	Quantity int         `json:"quantity"`
}

func apiGet(path string) ([]byte, error) {
	req, _ := http.NewRequest("GET", accountURL+path, nil)
	basePath, _, _ := strings.Cut(path, "?")
	setAuth(req, "GET", basePath)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// initSpent fetches existing open buy orders and seeds spentBits so the budget
// cap is respected even after a restart with open positions.
func initSpentFromAPI() {
	raw, err := apiGet("/v1/orders/open?marketSlug=" + slug)
	if err != nil {
		log.Printf("[INIT] could not fetch open orders: %v", err)
		return
	}
	if len(raw) == 0 || (raw[0] != '{' && raw[0] != '[') {
		log.Printf("[INIT] unexpected open orders response: %s", raw)
		return
	}
	var orders []openOrder
	if raw[0] == '[' {
		json.Unmarshal(raw, &orders)
	} else {
		var wrapped struct {
			Orders []openOrder `json:"orders"`
		}
		if err := json.Unmarshal(raw, &wrapped); err == nil {
			orders = wrapped.Orders
		}
	}
	if len(orders) == 0 {
		log.Printf("[INIT] no open positions in %s", slug)
		return
	}
	var committed float64
	for _, o := range orders {
		if !strings.Contains(o.Intent, "BUY") {
			continue
		}
		price, _ := strconv.ParseFloat(o.Price.Value, 64)
		if price == 0 {
			continue
		}
		cost := price * float64(o.Quantity)
		committed += cost
		log.Printf("[INIT]   order %s intent=%s qty=%d price=%.4f cost=$%.2f",
			o.ID, o.Intent, o.Quantity, price, cost)
	}
	if committed > 0 {
		addSpent(committed)
		log.Printf("[INIT] pre-existing commitment $%.2f — available $%.2f of $%.2f budget",
			committed, budget-loadSpent(), budget)
	} else {
		log.Printf("[INIT] no open buy positions — full $%.2f available", budget)
	}
}

// ── Snipe logic ───────────────────────────────────────────────────────────────

var buyInFlight atomic.Bool

func onBook(s snap) {
	if len(s.bids) == 0 || len(s.asks) == 0 {
		return
	}
	if buyInFlight.Load() {
		return
	}

	bestBid := s.bids[0].price

	// Scan all ask levels for mispricings (asks sorted low→high, so cheapest first).
	for _, ask := range s.asks {
		edge := bestBid - ask.price
		if edge < discount {
			// Asks are sorted ascending — once we pass the discount threshold
			// we can break early since remaining asks are even more expensive.
			break
		}
		avail := budget - loadSpent()
		spend := fmin(maxPerTrade, avail)
		qty := int(spend / ask.price)
		if qty < 1 {
			log.Printf("[SKIP] budget exhausted spent=$%.2f budget=$%.2f", loadSpent(), budget)
			return
		}
		log.Printf("[SNIPE] ask=%.4f bid=%.4f edge=+%.4f qty=%d cost=$%.2f",
			ask.price, bestBid, edge, qty, ask.price*float64(qty))
		if buyInFlight.CompareAndSwap(false, true) {
			go execSnipe(ask.price, bestBid, qty)
		}
		return // one snipe in flight at a time
	}
}

func execSnipe(buyPrice, sellPrice float64, qty int) {
	defer buyInFlight.Store(false)

	buyID, err := placeOrder("ORDER_INTENT_BUY_LONG", buyPrice, qty)
	if err != nil {
		log.Printf("[SNIPE ERR buy] %v", err)
		return
	}
	addSpent(buyPrice * float64(qty))
	log.Printf("[SNIPE BUY ] order=%s price=%.4f qty=%d cost=$%.2f",
		buyID, buyPrice, qty, buyPrice*float64(qty))

	// Immediately sell at best bid to lock in profit.
	sellID, err := placeOrder("ORDER_INTENT_SELL_LONG", sellPrice, qty)
	if err != nil {
		log.Printf("[SNIPE ERR sell] %v — position open at %.4f x%d!", err, buyPrice, qty)
		return
	}
	profit := (sellPrice - buyPrice) * float64(qty)
	addSpent(-(buyPrice * float64(qty))) // return capital
	log.Printf("[SNIPE SELL] order=%s profit=$%.4f", sellID, profit)
}

// ── WebSocket ─────────────────────────────────────────────────────────────────

func run() {
	for {
		if err := connect(); err != nil {
			log.Printf("[WS] %v — reconnect in 500ms", err)
			time.Sleep(500 * time.Millisecond)
		}
	}
}

func connect() error {
	ts, sig := sign("GET", "/v1/ws/markets")
	headers := http.Header{
		"X-PM-Access-Key": {keyID},
		"X-PM-Timestamp":  {ts},
		"X-PM-Signature":  {sig},
	}
	dialer := websocket.Dialer{
		ReadBufferSize:   65536,
		WriteBufferSize:  4096,
		HandshakeTimeout: 5 * time.Second,
	}
	conn, _, err := dialer.Dial(wsURL, headers)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	conn.SetReadLimit(1 << 20)

	err = conn.WriteJSON(map[string]any{
		"subscribe": map[string]any{
			"requestId":        "greedy-1",
			"subscriptionType": "SUBSCRIPTION_TYPE_MARKET_DATA",
			"marketSlugs":      []string{slug},
		},
	})
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	log.Printf("[WS] connected — watching %s  budget=$%.2f  discount≥%.2f  max-trade=$%.2f",
		slug, budget, discount, maxPerTrade)

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		if s, ok := parseBook(data); ok {
			onBook(s)
		}
	}
}

// ── Env loader ────────────────────────────────────────────────────────────────

func loadEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok {
			os.Setenv(strings.TrimSpace(k), strings.TrimSpace(v))
		}
	}
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	flag.Float64Var(&budget, "budget", 50.0, "total USD budget")
	flag.Float64Var(&discount, "discount", 0.015, "min gap below best bid to trigger snipe")
	flag.Float64Var(&maxPerTrade, "max-trade", 20.0, "max USD per single snipe")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: greedy_bin <market-slug> [--discount N] [--budget N] [--max-trade N]")
		os.Exit(1)
	}
	slug = flag.Arg(0)

	home, _ := os.UserHomeDir()
	loadEnv(".env")
	loadEnv("../.env")
	loadEnv("../modelgopher/.env")
	loadEnv(home + "/Desktop/Code/modelgopher/modelgopher/.env")

	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Printf("Greedy  market=%s  budget=$%.2f  discount≥%.2f  max-trade=$%.2f",
		slug, budget, discount, maxPerTrade)

	initAuth()
	initSpentFromAPI()
	run()
}
