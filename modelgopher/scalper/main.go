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

// checkExistingPositions fetches open orders for the slug, initialises
// spentBits so the budget cap applies correctly from the very first tick,
// and sells any filled positions that are already profitable.
func checkExistingPositions(bestBid, _ float64) {
	raw, err := apiGet("/v1/orders/open?marketSlug=" + slug)
	if err != nil {
		log.Printf("[INIT] could not fetch open orders: %v", err)
		return
	}
	if len(raw) > 0 && raw[0] != '{' && raw[0] != '[' {
		log.Printf("[INIT] open orders response: %s", raw)
		return
	}
	var result struct {
		Orders []openOrder `json:"orders"`
	}
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

	// ── Step 1: reserve budget for every open buy order ──────────────────────
	// We count the full order quantity (filled + pending) because both halves
	// tie up real capital: filled = contracts we hold, pending = capital the
	// exchange is holding for limit orders that haven't filled yet.
	for _, o := range result.Orders {
		if !strings.Contains(o.Intent, "BUY") {
			continue // sell orders don't consume budget
		}
		entryPrice, _ := strconv.ParseFloat(o.Price.Value, 64)
		if entryPrice == 0 {
			continue
		}
		committed := entryPrice * float64(o.Quantity)
		addSpent(committed)
		log.Printf("[INIT]   order %s intent=%s qty=%d filled=%d price=%.4f committed=$%.2f",
			o.ID, o.Intent, o.Quantity, o.Filled, entryPrice, committed)
	}
	log.Printf("[INIT] pre-existing commitment $%.2f — available $%.2f of $%.2f budget",
		loadSpent(), budget-loadSpent(), budget)

	// ── Step 2: sell profitable filled positions ──────────────────────────────
	for _, o := range result.Orders {
		if !strings.Contains(o.Intent, "BUY") || o.Filled == 0 {
			continue
		}
		entryPrice, _ := strconv.ParseFloat(o.Price.Value, 64)
		if entryPrice == 0 {
			continue
		}
		profit := (bestBid - entryPrice) * float64(o.Filled)
		log.Printf("[INIT]   order %s: filled=%d bid=%.4f profit=$%.4f",
			o.ID, o.Filled, bestBid, profit)
		if profit >= minProfit*float64(o.Filled) {
			sellIntent := "ORDER_INTENT_SELL_LONG"
			if strings.Contains(o.Intent, "SHORT") {
				sellIntent = "ORDER_INTENT_SELL_SHORT"
			}
			log.Printf("[INIT]   → selling %d contracts for +$%.4f", o.Filled, profit)
			id, err := placeOrder(sellIntent, bestBid, o.Filled)
			if err != nil {
				log.Printf("[INIT]   sell ERR: %v", err)
			} else {
				// Return the full committed budget for this order now that it's closed.
				addSpent(-(entryPrice * float64(o.Quantity)))
				log.Printf("[INIT]   sell OK: order=%s  freed $%.2f  available $%.2f",
					id, entryPrice*float64(o.Quantity), budget-loadSpent())
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
	intent      string // ORDER_INTENT_BUY_LONG or ORDER_INTENT_BUY_SHORT
	price       float64
	qty         int
	orderID     string
	sellOrderID string // non-empty if a GTC sell limit is already on the book
}

var posPtr        atomic.Pointer[position]
var buyInFlight   atomic.Bool // true while an execBuy goroutine is running
var totalBuys     atomic.Int64
var totalSells    atomic.Int64
var forceSell     atomic.Bool  // set by stdin goroutine to trigger immediate sell
var lastStatusNs  atomic.Int64 // nanoseconds of last status line print

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

	// ── Live status line (stdout, \r so it overwrites itself) ─────────────────
	if p := posPtr.Load(); p != nil {
		now := time.Now().UnixNano()
		if now-lastStatusNs.Load() > 150_000_000 { // update every 150ms max
			lastStatusNs.Store(now)
			var pnl float64
			side := "YES"
			if p.intent == "ORDER_INTENT_BUY_LONG" {
				pnl = (bestBid - p.price) * float64(p.qty)
			} else {
				side = "NO"
				pnl = ((1.0 - bestAsk) - p.price) * float64(p.qty)
			}
			sign := "+"
			if pnl < 0 {
				sign = ""
			}
			fmt.Printf("\r\033[K  [LIVE] %s x%d @ %.4f  bid=%.4f ask=%.4f  P&L=%s$%.4f  [f+Enter=force sell]",
				side, p.qty, p.price, bestBid, bestAsk, sign, pnl)
		}
	}

	// ── Force sell triggered by user ──────────────────────────────────────────
	if forceSell.CompareAndSwap(true, false) {
		p := posPtr.Load()
		if p == nil {
			log.Printf("\n[FORCE SELL] no open position")
			return
		}
		fmt.Println() // end the \r status line cleanly
		var sellIntent string
		var sellPrice float64
		if p.intent == "ORDER_INTENT_BUY_LONG" {
			sellIntent = "ORDER_INTENT_SELL_LONG"
			sellPrice = bestBid
		} else {
			sellIntent = "ORDER_INTENT_SELL_SHORT"
			sellPrice = bestAsk
		}
		// Cancel any queued GTC sell so we don't get a duplicate fill.
		if p.sellOrderID != "" {
			if sc, err := apiDelete("/v1/orders/" + p.sellOrderID); err != nil {
				log.Printf("[CANCEL ERR] %v", err)
			} else {
				log.Printf("[CANCEL] GTC sell %s cancelled (status %d)", p.sellOrderID, sc)
				p.sellOrderID = ""
			}
		}
		log.Printf("[FORCE SELL] %s at %.4f", sellIntent, sellPrice)
		go execSell(p, sellIntent, sellPrice)
		return
	}

	// One-time startup: check for existing positions and seed spentBits.
	// Hold buyInFlight true for the duration so no trade fires before the
	// check finishes populating spentBits — otherwise the very next frame
	// sees spentBits=0 and thinks the full budget is free.
	if initDone.CompareAndSwap(false, true) {
		buyInFlight.Store(true)
		go func() {
			checkExistingPositions(bestBid, bestAsk)
			buyInFlight.Store(false)
		}()
		return
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
			// Sell when total P&L across all contracts >= minProfit
			if (bestBid-p.price)*float64(p.qty) >= minProfit {
				go execSell(p, "ORDER_INTENT_SELL_LONG", bestBid)
			}
		} else { // SHORT (long NO)
			// Total P&L = improvement in NO value * qty
			if ((1.0-bestAsk)-p.price)*float64(p.qty) >= minProfit {
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
		log.Printf("[ARB ] combined=%.4f  YES ask=%.4f BB=%s", bestAsk+(1-bestBid), bestAsk, bbStr)
		if buyInFlight.CompareAndSwap(false, true) {
			go execBuy("ORDER_INTENT_BUY_LONG", bestAsk, bestAsk)
		}
		return
	}

	// Scalp: tight spread + order book imbalance + BB filter
	if spread > enterSpread {
		return
	}
	bidDepth := depth(s.bids, 3)
	askDepth := depth(s.asks, 3)

	if bidDepth > askDepth*1.5 {
		// Long YES — skip if mid is above upper band (price stretched high)
		if bbReady && mid > bbUpper {
			log.Printf("[SKIP] BB overbought mid=%.4f upper=%.4f", mid, bbUpper)
			return
		}
		log.Printf("[BUY ] YES ask=%.4f spread=%.4f bid/ask=%.0f/%.0f BB=%s",
			bestAsk, spread, bidDepth, askDepth, bbStr)
		if buyInFlight.CompareAndSwap(false, true) {
			// LONG: order price = YES ask, cost per contract = YES ask
			go execBuy("ORDER_INTENT_BUY_LONG", bestAsk, bestAsk)
		}
	} else if askDepth > bidDepth*1.5 {
		// Short YES (long NO) — skip if mid is below lower band (price stretched low)
		if bbReady && mid < bbLower {
			log.Printf("[SKIP] BB oversold mid=%.4f lower=%.4f", mid, bbLower)
			return
		}
		noPrice := 1.0 - bestAsk
		log.Printf("[BUY ] NO  price=%.4f spread=%.4f bid/ask=%.0f/%.0f BB=%s",
			noPrice, spread, bidDepth, askDepth, bbStr)
		if buyInFlight.CompareAndSwap(false, true) {
			// SHORT: order price = YES ask (what the exchange sees),
			// cost per contract = NO price (1 - YES ask) — what we actually pay.
			go execBuy("ORDER_INTENT_BUY_SHORT", bestAsk, noPrice)
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

// execBuy places a limit buy order and updates the budget tracker.
// orderPrice is the price sent to the API (always the YES ask for the exchange).
// costPrice is the per-contract cost for budget tracking:
//   - LONG: same as orderPrice (YES ask)
//   - SHORT: 1 - orderPrice  (NO price, what you actually pay per contract)
func execBuy(intent string, orderPrice, costPrice float64) {
	defer buyInFlight.Store(false)

	avail := budget - loadSpent()
	spend := fmin(maxPerTrade, avail)
	qty := int(spend / costPrice)
	if qty < 1 {
		log.Printf("[SKIP] budget exhausted spent=$%.2f budget=$%.2f", loadSpent(), budget)
		return
	}
	cost := costPrice * float64(qty)
	if loadSpent()+cost > budget+0.01 { // 1-cent tolerance for float rounding
		log.Printf("[SKIP] would exceed budget: spent=%.2f + cost=%.2f > budget=%.2f",
			loadSpent(), cost, budget)
		return
	}
	id, err := placeOrder(intent, orderPrice, qty)
	if err != nil {
		log.Printf("[BUY ERR] %v", err)
		return
	}
	p := &position{intent: intent, price: costPrice, qty: qty, orderID: id}
	posPtr.Store(p)
	addSpent(cost)
	totalBuys.Add(1)
	log.Printf("[BUY OK] %s  order=%s  qty=%d  cost=$%.2f  spent=$%.2f/%.2f  round=%d",
		intent, id, qty, cost, loadSpent(), budget, totalBuys.Load())

	// Immediately post a GTC sell limit at the minimum profit target.
	// This sits on the exchange book and fills the instant someone crosses it —
	// no need to wait for a WS tick to confirm price moved.
	var sellIntent string
	var sellPrice float64
	// minProfit is total USD — convert to per-contract price delta.
	perContract := minProfit / float64(qty)
	if intent == "ORDER_INTENT_BUY_LONG" {
		sellIntent = "ORDER_INTENT_SELL_LONG"
		sellPrice = costPrice + perContract
	} else {
		// SHORT: YES ask must fall by perContract from original YES ask (1 - costPrice)
		sellIntent = "ORDER_INTENT_SELL_SHORT"
		sellPrice = (1.0 - costPrice) - perContract
	}
	sellID, sellErr := placeOrder(sellIntent, sellPrice, qty)
	if sellErr != nil {
		log.Printf("[SELL QUEUE ERR] %v — onBook monitor will retry", sellErr)
	} else {
		p.sellOrderID = sellID
		log.Printf("[SELL QUEUED] %s @ %.4f  order=%s", sellIntent, sellPrice, sellID)
	}
}

func execSell(p *position, intent string, price float64) {
	if !posPtr.CompareAndSwap(p, nil) {
		return // another goroutine beat us
	}
	fmt.Println() // end the live \r status line before logging

	var orderID string
	if p.sellOrderID != "" {
		// GTC sell already on the book from execBuy — price crossed, treat as filled.
		orderID = p.sellOrderID
		log.Printf("[SELL CONFIRM] GTC order %s filled at ~%.4f", orderID, price)
	} else {
		// Fallback: no queued sell, place one now.
		id, err := placeOrder(intent, price, p.qty)
		if err != nil {
			log.Printf("[SELL ERR] %v — restoring", err)
			posPtr.Store(p)
			return
		}
		orderID = id
	}

	var profit float64
	if strings.Contains(intent, "SELL_LONG") {
		// p.price = YES costPrice; profit = price improvement per contract
		profit = (price - p.price) * float64(p.qty)
	} else {
		// p.price = NO costPrice; price = current YES ask
		// NO value at sell = 1 - price; profit per contract = (1-price) - p.price
		profit = ((1.0 - price) - p.price) * float64(p.qty)
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
		intent, orderID, profit, totalBuys.Load(), totalSells.Load(), coolMsg)
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
	flag.Float64Var(&budget, "budget", 40.0, "total USD budget")
	flag.Float64Var(&minProfit, "profit", 0.20, "min total profit in USD to trigger sell")
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

	// Send logs to stderr so they don't clobber the \r live status line on stdout.
	log.SetOutput(os.Stderr)
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Printf("Scalper  market=%s  budget=$%.2f  profit≥$%.2f total  spread≤%.2f  max/trade=$%.2f",
		slug, budget, minProfit, enterSpread, maxPerTrade)
	fmt.Println("  Type 'f' + Enter at any time to force-sell the open position.")

	// Goroutine: read stdin for 'f'+Enter → set forceSell flag.
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) == "f" {
				forceSell.Store(true)
			}
		}
	}()

	initAuth()
	run()
}
