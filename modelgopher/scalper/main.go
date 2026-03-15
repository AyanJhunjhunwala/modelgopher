// Scalper — live order-book scalper for a single Polymarket US market.
// Usage: ./scalper_bin <market-slug> [flags]
//
//	./scalper_bin aec-nba-den-lal-2026-03-14
//	./scalper_bin aec-nba-den-lal-2026-03-14 --budget 10 --profit 0.01
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
	"math"
	"sync"
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
	slug        string
	budget      float64
	minProfit   float64
	enterSpread float64
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

func apiGet(path string) ([]byte, error) {
	req, _ := http.NewRequest("GET", accountURL+path, nil)
	// Sign only the base path, not query string.
	basePath, _, _ := strings.Cut(path, "?")
	setAuth(req, "GET", basePath)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
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

func apiDelete(path string) (int, error) {
	req, _ := http.NewRequest("DELETE", accountURL+path, nil)
	setAuth(req, "DELETE", path)
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// ── Open position check ───────────────────────────────────────────────────────

type nestedPrice struct {
	Value string `json:"value"`
}

type openOrder struct {
	ID       string      `json:"id"`
	Intent   string      `json:"intent"`
	Price    nestedPrice `json:"price"`
	Quantity int         `json:"quantity"`
	Filled   int         `json:"filledQuantity"`
	Status   string      `json:"status"`
}

// checkExistingPositions fetches open orders for the slug and sells any that
// are already profitable at the current best bid.
func checkExistingPositions(bestBid, _ float64) {
	raw, err := apiGet("/v1/orders/open?marketSlug=" + slug)
	if err != nil {
		log.Printf("[INIT] could not fetch open orders: %v", err)
		return
	}
	// Log raw for debugging if not valid JSON object
	if len(raw) > 0 && raw[0] != '{' && raw[0] != '[' {
		log.Printf("[INIT] open orders response: %s", raw)
		return
	}
	var result struct {
		Orders []openOrder `json:"orders"`
	}
	// API may return a top-level array instead of wrapped object
	if len(raw) > 0 && raw[0] == '[' {
		json.Unmarshal(raw, &result.Orders)
	} else if err := json.Unmarshal(raw, &result); err != nil {
		log.Printf("[INIT] parse open orders: %v — raw: %s", err, raw)
		return
	}
	if len(result.Orders) == 0 {
		log.Printf("[INIT] no open positions in %s", slug)
		return
	}
	log.Printf("[INIT] found %d open order(s) in %s", len(result.Orders), slug)
	for _, o := range result.Orders {
		filled := o.Filled
		if filled == 0 {
			log.Printf("[INIT]   order %s: unfilled — leaving on book", o.ID)
			continue
		}
		entryPrice, _ := strconv.ParseFloat(o.Price.Value, 64)
		profit := (bestBid - entryPrice) * float64(filled)
		log.Printf("[INIT]   order %s: intent=%s price=%.4f filled=%d bid=%.4f profit=$%.4f",
			o.ID, o.Intent, entryPrice, filled, bestBid, profit)
		if profit >= minProfit*float64(filled) && entryPrice > 0 {
			log.Printf("[INIT]   → selling %d contracts for +$%.4f", filled, profit)
			sellIntent := "ORDER_INTENT_SELL_LONG"
			if strings.Contains(o.Intent, "SHORT") {
				sellIntent = "ORDER_INTENT_SELL_SHORT"
			}
			id, err := placeOrder(sellIntent, bestBid, filled)
			if err != nil {
				log.Printf("[INIT]   sell ERR: %v", err)
			} else {
				log.Printf("[INIT]   sell OK: order=%s", id)
			}
		}
	}
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

// ── Position — lock-free ──────────────────────────────────────────────────────

type position struct {
	intent  string // ORDER_INTENT_BUY_LONG or ORDER_INTENT_BUY_SHORT
	price   float64
	qty     int
	orderID string
}

var posPtr       atomic.Pointer[position]
var buyInFlight  atomic.Bool // true while an execBuy goroutine is running
var totalBuys    atomic.Int64
var totalSells   atomic.Int64

// atomic float64 spent tracker via unsafe bit cast
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

// ── Bollinger Bands + cooldown ────────────────────────────────────────────────

const (
	bbPeriod = 20    // lookback window for mean/stddev
	bbMult   = 1.5   // band width multiplier
	bbMaxStd = 0.05  // skip entry if stddev exceeds this (too chaotic)
	histLen  = 64    // ring buffer length (must be > bbPeriod)
)

var cooldownSec int // set from flag in main()

var (
	priceRing  [histLen]float64
	ringIdx    int
	ringCount  int
	ringMu     sync.Mutex
	lastSellAt atomic.Int64 // unix seconds of most recent sell
)

func pushMid(mid float64) {
	ringMu.Lock()
	priceRing[ringIdx%histLen] = mid
	ringIdx++
	if ringCount < histLen {
		ringCount++
	}
	ringMu.Unlock()
}

// bollingerBands returns (mean, upper, lower, stddev, ready).
// ready=false means not enough data yet (< bbPeriod ticks).
func bollingerBands() (mean, upper, lower, stddev float64, ready bool) {
	ringMu.Lock()
	defer ringMu.Unlock()
	if ringCount < bbPeriod {
		return
	}
	var sum float64
	for i := 0; i < bbPeriod; i++ {
		sum += priceRing[(ringIdx-bbPeriod+i+histLen)%histLen]
	}
	mean = sum / bbPeriod
	var variance float64
	for i := 0; i < bbPeriod; i++ {
		d := priceRing[(ringIdx-bbPeriod+i+histLen)%histLen] - mean
		variance += d * d
	}
	stddev = math.Sqrt(variance / bbPeriod)
	upper = mean + bbMult*stddev
	lower = mean - bbMult*stddev
	ready = true
	return
}

func coolingDown() bool {
	if cooldownSec <= 0 {
		return false
	}
	// Only cool down when fully flat (all bought positions have been sold).
	// If sells < buys we still have open exposure — no cooldown needed.
	if totalSells.Load() < totalBuys.Load() {
		return false
	}
	return time.Now().Unix()-lastSellAt.Load() < int64(cooldownSec)
}

// ── Decision logic ────────────────────────────────────────────────────────────

var initDone atomic.Bool

func onBook(s snap) {
	if len(s.bids) == 0 || len(s.asks) == 0 {
		return
	}
	bestBid := s.bids[0].price
	bestAsk := s.asks[0].price
	spread := bestAsk - bestBid
	mid := (bestBid + bestAsk) / 2

	pushMid(mid)

	// One-time startup: check for existing profitable positions.
	if initDone.CompareAndSwap(false, true) {
		go checkExistingPositions(bestBid, bestAsk)
	}

	avail := budget - loadSpent()

	// ── Sell / entry gate ────────────────────────────────────────────────────
	// Block if a buy is already in flight — posPtr not set yet but money committed.
	if buyInFlight.Load() {
		return
	}

	// ── Sell ─────────────────────────────────────────────────────────────────
	if p := posPtr.Load(); p != nil {
		if p.intent == "ORDER_INTENT_BUY_LONG" {
			if bestBid >= p.price+minProfit {
				go execSell(p, "ORDER_INTENT_SELL_LONG", bestBid)
			}
		} else { // SHORT (long NO)
			if bestAsk <= p.price-minProfit {
				go execSell(p, "ORDER_INTENT_SELL_SHORT", bestAsk)
			}
		}
		return // one position at a time
	}

	// ── Entry ─────────────────────────────────────────────────────────────────
	if avail < 0.05 {
		return
	}

	// Cooldown: don't re-enter too soon after a sell.
	if coolingDown() {
		secs := int64(cooldownSec) - (time.Now().Unix() - lastSellAt.Load())
		log.Printf("[SKIP] cooldown %ds remaining", secs)
		return
	}

	bbMean, bbUpper, bbLower, bbStd, bbReady := bollingerBands()
	bbStr := "warming"
	if bbReady {
		bbStr = fmt.Sprintf("mean=%.4f ±%.4f", bbMean, bbStd)
	}

	// High volatility guard — market is chaotic, skip all entries.
	if bbReady && bbStd > bbMaxStd {
		log.Printf("[SKIP] volatile stddev=%.4f > %.4f", bbStd, bbMaxStd)
		return
	}

	// Arb: cost of YES+NO < $1 guaranteed — bypass BB (pure math edge)
	if bestAsk+(1.0-bestBid) < 0.99 {
		qty := int(fmin(maxPerTrade, avail) / bestAsk)
		if qty >= 1 {
			log.Printf("[ARB ] combined=%.4f  YES ask=%.4f qty=%d BB=%s", bestAsk+(1-bestBid), bestAsk, qty, bbStr)
			if buyInFlight.CompareAndSwap(false, true) {
				go execBuy("ORDER_INTENT_BUY_LONG", bestAsk, qty)
			}
		}
		return
	}

	// Scalp: tight spread + order book imbalance + BB filter
	if spread > enterSpread {
		return
	}
	bidDepth := depth(s.bids, 3)
	askDepth := depth(s.asks, 3)
	spend := fmin(maxPerTrade, avail)

	if bidDepth > askDepth*1.5 {
		// Long YES — skip if mid is above upper band (price stretched high)
		if bbReady && mid > bbUpper {
			log.Printf("[SKIP] BB overbought mid=%.4f upper=%.4f", mid, bbUpper)
			return
		}
		qty := int(spend / bestAsk)
		if qty >= 1 {
			log.Printf("[BUY ] YES ask=%.4f qty=%d spread=%.4f bid/ask=%.0f/%.0f BB=%s",
				bestAsk, qty, spread, bidDepth, askDepth, bbStr)
			if buyInFlight.CompareAndSwap(false, true) {
				go execBuy("ORDER_INTENT_BUY_LONG", bestAsk, qty)
			}
		}
	} else if askDepth > bidDepth*1.5 {
		// Short YES (long NO) — skip if mid is below lower band (price stretched low)
		if bbReady && mid < bbLower {
			log.Printf("[SKIP] BB oversold mid=%.4f lower=%.4f", mid, bbLower)
			return
		}
		noPrice := 1.0 - bestAsk
		qty := int(spend / noPrice)
		if qty >= 1 {
			log.Printf("[BUY ] NO  price=%.4f qty=%d spread=%.4f bid/ask=%.0f/%.0f BB=%s",
				noPrice, qty, spread, bidDepth, askDepth, bbStr)
			if buyInFlight.CompareAndSwap(false, true) {
				go execBuy("ORDER_INTENT_BUY_SHORT", bestAsk, qty)
			}
		}
	}
}

// fetchLiveBalance returns the account's current buying power from the API.
func fetchLiveBalance() float64 {
	raw, err := apiGet("/v1/account/balances")
	if err != nil {
		return 0
	}
	var result struct {
		Balances []struct {
			BuyingPower float64 `json:"buyingPower"`
		} `json:"balances"`
	}
	if json.Unmarshal(raw, &result) != nil || len(result.Balances) == 0 {
		return 0
	}
	return result.Balances[0].BuyingPower
}

func execBuy(intent string, price float64, _ int) {
	defer buyInFlight.Store(false)

	avail := budget - loadSpent()
	spend := fmin(maxPerTrade, avail)
	qty := int(spend / price)
	if qty < 1 {
		log.Printf("[SKIP] budget exhausted spent=$%.2f budget=$%.2f", loadSpent(), budget)
		return
	}
	id, err := placeOrder(intent, price, qty)
	if err != nil {
		log.Printf("[BUY ERR] %v", err)
		return
	}
	p := &position{intent: intent, price: price, qty: qty, orderID: id}
	posPtr.Store(p)
	addSpent(price * float64(qty))
	totalBuys.Add(1)
	log.Printf("[BUY OK] %s  order=%s  qty=%d  cost=$%.2f  spent=$%.2f/%.2f  round=%d",
		intent, id, qty, price*float64(qty), loadSpent(), budget, totalBuys.Load())
}

func execSell(p *position, intent string, price float64) {
	if !posPtr.CompareAndSwap(p, nil) {
		return // another goroutine beat us
	}
	id, err := placeOrder(intent, price, p.qty)
	if err != nil {
		log.Printf("[SELL ERR] %v — restoring", err)
		posPtr.Store(p)
		return
	}
	var profit float64
	if strings.Contains(intent, "SELL_LONG") {
		profit = (price - p.price) * float64(p.qty) // LONG: sold higher than bought
	} else {
		profit = (p.price - price) * float64(p.qty) // SHORT: YES dropped, NO gained
	}
	addSpent(-(p.price * float64(p.qty))) // return capital to budget tracker
	totalSells.Add(1)
	flat := totalSells.Load() >= totalBuys.Load()
	lastSellAt.Store(time.Now().Unix())
	coolMsg := "no cooldown (still open positions)"
	if flat {
		coolMsg = fmt.Sprintf("cooldown %ds starting", cooldownSec)
	}
	log.Printf("[SELL OK] %s  order=%s  profit=$%.4f  buys=%d sells=%d  %s",
		intent, id, profit, totalBuys.Load(), totalSells.Load(), coolMsg)
}

func depth(levels []level, n int) float64 {
	var t float64
	for i := 0; i < n && i < len(levels); i++ {
		t += levels[i].qty
	}
	return t
}

func fmin(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
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
			"requestId":        "scalper-1",
			"subscriptionType": "SUBSCRIPTION_TYPE_MARKET_DATA",
			"marketSlugs":      []string{slug},
		},
	})
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	log.Printf("[WS] connected — watching %s  budget=$%.2f  spread≤%.2f  profit≥$%.2f",
		slug, budget, enterSpread, minProfit)

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
	// Flags
	flag.Float64Var(&budget, "budget", 20.0, "total USD budget")
	flag.Float64Var(&minProfit, "profit", 0.02, "min profit per contract to sell")
	flag.Float64Var(&enterSpread, "spread", 0.02, "max spread to enter")
	flag.Float64Var(&maxPerTrade, "max-trade", 20.0, "max USD per single entry")
	flag.IntVar(&cooldownSec, "cooldown", 5, "seconds to wait after a sell before re-entry")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: scalper_bin <market-slug> [--budget N] [--profit N] [--spread N] [--max-trade N]")
		os.Exit(1)
	}
	slug = flag.Arg(0)

	// Load credentials
	home, _ := os.UserHomeDir()
	loadEnv(".env")
	loadEnv("../.env")
	loadEnv("../modelgopher/.env")
	loadEnv(home + "/Desktop/Code/modelgopher/modelgopher/.env")

	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Printf("Scalper  market=%s  budget=$%.2f  profit≥$%.2f  spread≤%.2f  max/trade=$%.2f",
		slug, budget, minProfit, enterSpread, maxPerTrade)

	initAuth()
	run()
}
