// Manual — interactive manual trader for a single Polymarket US market.
// Press y to buy YES, n to buy NO, enter a sell-threshold, then sit back
// and let the bot auto-sell when the price hits your target.
//
// Usage: ./manual_bin <market-slug> [--budget N]
//
//	./manual_bin aec-nba-den-lal-2026-03-14
//	./manual_bin aec-nba-den-lal-2026-03-14 --budget 50
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
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/gorilla/websocket"
)

// ── Constants ──────────────────────────────────────────────────────────────────

const (
	accountURL = "https://api.polymarket.us"
	wsURL      = "wss://api.polymarket.us/v1/ws/markets"
)

// ── Auth ──────────────────────────────────────────────────────────────────────

var (
	privKey ed25519.PrivateKey
	keyID   string
)

func initAuth() {
	raw, err := base64.StdEncoding.DecodeString(os.Getenv("PM_SECRET"))
	if err != nil || len(raw) < 32 {
		fmt.Fprintln(os.Stderr, "bad PM_SECRET — check your .env")
		os.Exit(1)
	}
	privKey = ed25519.NewKeyFromSeed(raw[:32])
	keyID = os.Getenv("PM_KEY_ID")
	if keyID == "" {
		fmt.Fprintln(os.Stderr, "PM_KEY_ID not set")
		os.Exit(1)
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

// ── HTTP ──────────────────────────────────────────────────────────────────────

var httpClient = &http.Client{Timeout: 8 * time.Second}

func apiPost(path string, body any) ([]byte, int, error) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", accountURL+path, bytes.NewReader(b))
	setAuth(req, "POST", path)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return raw, resp.StatusCode, nil
}

// ── Existing position check ───────────────────────────────────────────────────

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
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// existingCommitment returns the total USD already committed in open buy orders
// for this slug. Reduces the effective budget so we don't double-spend.
func existingCommitment(slug string) float64 {
	raw, err := apiGet("/v1/orders/open?marketSlug=" + slug)
	if err != nil {
		return 0
	}
	if len(raw) == 0 || (raw[0] != '{' && raw[0] != '[') {
		return 0
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
	var total float64
	for _, o := range orders {
		if !strings.Contains(o.Intent, "BUY") {
			continue
		}
		price, _ := strconv.ParseFloat(o.Price.Value, 64)
		total += price * float64(o.Quantity)
	}
	return total
}

// ── Order placement ───────────────────────────────────────────────────────────

func placeOrder(slug, intent string, price float64, qty int) (string, error) {
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

// ── Order book types ──────────────────────────────────────────────────────────

type level struct{ price, qty float64 }
type snap struct{ bids, asks []level }

// ── Position ──────────────────────────────────────────────────────────────────

type position struct {
	intent     string // ORDER_INTENT_BUY_LONG or ORDER_INTENT_BUY_SHORT
	entryPrice float64
	qty        int
	orderID    string
	threshold  float64
}

// ── App states ────────────────────────────────────────────────────────────────

type appState int

const (
	stateWatch     appState = iota // live order book, waiting for user action
	stateThreshold                 // user entered y/n, typing sell threshold
	stateBuying                    // buy order in flight
	stateActive                    // position open, monitoring threshold
	stateSelling                   // sell order in flight
	stateDone                      // trade complete, show result
)

// ── Messages ──────────────────────────────────────────────────────────────────

type bookMsg snap

type buyResultMsg struct {
	orderID string
	price   float64
	qty     int
	intent  string
	err     error
}

type sellResultMsg struct {
	orderID string
	profit  float64
	err     error
}

// ── Styles ────────────────────────────────────────────────────────────────────

var (
	styleTitle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	styleDim     = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	styleYes     = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)
	styleNo      = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	styleBid     = lipgloss.NewStyle().Foreground(lipgloss.Color("45"))
	styleAsk     = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	styleGreen   = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	styleRed     = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	styleWarning = lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Bold(true)
)

// ── Model ─────────────────────────────────────────────────────────────────────

type model struct {
	slug        string
	budget      float64
	state       appState
	ob          snap
	updated     time.Time
	width       int
	height      int
	pendingBuy  string // "yes" or "no" while in stateThreshold
	threshInput textinput.Model
	threshold   float64 // sell threshold entered by user
	pos         *position
	logs        []string
	bookCh      chan snap
	existing    float64 // USD already committed in open orders at startup
	profit      float64
	errMsg      string
}

func initialModel(slug string, budget float64, bookCh chan snap) model {
	ti := textinput.New()
	ti.Placeholder = "e.g. 0.65"
	ti.Width = 20
	return model{
		slug:        slug,
		budget:      budget,
		state:       stateWatch,
		bookCh:      bookCh,
		threshInput: ti,
	}
}

func (m model) Init() tea.Cmd {
	return waitBookCmd(m.bookCh)
}

// ── Bubble Tea commands ───────────────────────────────────────────────────────

func waitBookCmd(ch chan snap) tea.Cmd {
	return func() tea.Msg {
		return bookMsg(<-ch)
	}
}

// available returns how much of the budget is free after pre-existing orders.
func (m model) available() float64 {
	a := m.budget - m.existing
	if a < 0 {
		return 0
	}
	return a
}

func buyCmd(slug, intent string, price, available float64) tea.Cmd {
	return func() tea.Msg {
		qty := int(available / price)
		if qty < 1 {
			return buyResultMsg{err: fmt.Errorf("no budget left ($%.2f available at %.4f)", available, price)}
		}
		id, err := placeOrder(slug, intent, price, qty)
		return buyResultMsg{id, price, qty, intent, err}
	}
}

func sellCmd(slug, intent string, price float64, pos *position) tea.Cmd {
	return func() tea.Msg {
		id, err := placeOrder(slug, intent, price, pos.qty)
		profit := 0.0
		if err == nil {
			if strings.Contains(intent, "SELL_LONG") {
				profit = (price - pos.entryPrice) * float64(pos.qty)
			} else {
				profit = (pos.entryPrice - price) * float64(pos.qty)
			}
		}
		return sellResultMsg{id, profit, err}
	}
}

// ── Update ────────────────────────────────────────────────────────────────────

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		switch m.state {
		case stateWatch:
			switch msg.String() {
			case "ctrl+c", "q":
				return m, tea.Quit
			case "y", "Y":
				m.pendingBuy = "yes"
				m.threshInput.SetValue("")
				m.threshInput.Focus()
				m.state = stateThreshold
				m.errMsg = ""
			case "n", "N":
				m.pendingBuy = "no"
				m.threshInput.SetValue("")
				m.threshInput.Focus()
				m.state = stateThreshold
				m.errMsg = ""
			}

		case stateThreshold:
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "esc":
				m.state = stateWatch
				m.pendingBuy = ""
				m.threshInput.Blur()
				m.errMsg = ""
			case "enter":
				val := strings.TrimSpace(m.threshInput.Value())
				thresh, err := strconv.ParseFloat(val, 64)
				if err != nil || thresh <= 0 || thresh > 1 {
					m.errMsg = "Invalid price — enter a value between 0 and 1 (e.g. 0.65)"
					return m, nil
				}
				m.errMsg = ""
				m.threshInput.Blur()

				var intent string
				var price float64
				if len(m.ob.asks) == 0 {
					m.errMsg = "No ask price available yet, try again"
					m.state = stateWatch
					return m, nil
				}
				price = m.ob.asks[0].price
				if m.pendingBuy == "yes" {
					intent = "ORDER_INTENT_BUY_LONG"
				} else {
					intent = "ORDER_INTENT_BUY_SHORT"
				}
				m.threshold = thresh
				m.state = stateBuying
				return m, buyCmd(m.slug, intent, price, m.available())
			default:
				var cmd tea.Cmd
				m.threshInput, cmd = m.threshInput.Update(msg)
				m.errMsg = ""
				return m, cmd
			}

		case stateActive:
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}

		case stateDone:
			return m, tea.Quit
		}

	case bookMsg:
		m.ob = snap(msg)
		m.updated = time.Now()

		// Check sell threshold when a position is active.
		if m.state == stateActive && m.pos != nil {
			bestBid := 0.0
			bestAsk := 0.0
			if len(m.ob.bids) > 0 {
				bestBid = m.ob.bids[0].price
			}
			if len(m.ob.asks) > 0 {
				bestAsk = m.ob.asks[0].price
			}

			var triggered bool
			var sellIntent string
			var sellPrice float64

			switch m.pos.intent {
			case "ORDER_INTENT_BUY_LONG":
				// Long YES: sell when bid ≥ threshold
				if bestBid >= m.pos.threshold {
					triggered = true
					sellIntent = "ORDER_INTENT_SELL_LONG"
					sellPrice = bestBid
				}
			case "ORDER_INTENT_BUY_SHORT":
				// Short YES (long NO): sell when YES ask ≤ threshold
				if bestAsk > 0 && bestAsk <= m.pos.threshold {
					triggered = true
					sellIntent = "ORDER_INTENT_SELL_SHORT"
					sellPrice = bestAsk
				}
			}

			if triggered {
				m.state = stateSelling
				m.addLog(fmt.Sprintf("Threshold hit! Selling at %.4f", sellPrice))
				return m, tea.Batch(
					waitBookCmd(m.bookCh),
					sellCmd(m.slug, sellIntent, sellPrice, m.pos),
				)
			}
		}
		return m, waitBookCmd(m.bookCh)

	case buyResultMsg:
		if msg.err != nil {
			m.errMsg = "Buy failed: " + msg.err.Error()
			m.state = stateWatch
			return m, nil
		}
		m.pos = &position{
			intent:     msg.intent,
			entryPrice: msg.price,
			qty:        msg.qty,
			orderID:    msg.orderID,
			threshold:  m.threshold,
		}
		// Persist the threshold that was set before buying.
		m.pos.threshold = m.threshold
		m.addLog(fmt.Sprintf("Bought %d contracts @ %.4f  order=%s", msg.qty, msg.price, msg.orderID))
		m.state = stateActive
		return m, nil

	case sellResultMsg:
		if msg.err != nil {
			m.errMsg = "Sell failed: " + msg.err.Error()
			m.state = stateActive // go back to monitoring
			return m, nil
		}
		m.profit = msg.profit
		m.addLog(fmt.Sprintf("Sold!  profit=$%.4f  order=%s", msg.profit, msg.orderID))
		m.state = stateDone
		return m, nil
	}

	// Pass remaining keys to textinput in threshold state.
	if m.state == stateThreshold {
		var cmd tea.Cmd
		m.threshInput, cmd = m.threshInput.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *model) addLog(s string) {
	m.logs = append(m.logs, time.Now().Format("15:04:05")+"  "+s)
	if len(m.logs) > 8 {
		m.logs = m.logs[len(m.logs)-8:]
	}
}

// ── View ──────────────────────────────────────────────────────────────────────

func depthBar(qty, maxQty float64, width int) string {
	if maxQty == 0 {
		return ""
	}
	n := int((qty / maxQty) * float64(width))
	if n < 0 {
		n = 0
	}
	if n > width {
		n = width
	}
	return strings.Repeat("█", n)
}

func (m model) View() string {
	var sb strings.Builder
	sb.WriteString("\n")

	// Header
	header := "  " + styleTitle.Render("Manual Trader") + "  " + styleDim.Render(m.slug)
	sb.WriteString(header + "\n\n")

	// ── Live order book ────────────────────────────────────────────────────────
	if len(m.ob.asks) > 0 || len(m.ob.bids) > 0 {
		// YES/NO probability line
		if len(m.ob.bids) > 0 && len(m.ob.asks) > 0 {
			bestBid := m.ob.bids[0].price
			bestAsk := m.ob.asks[0].price
			mid := (bestBid + bestAsk) / 2
			sb.WriteString(fmt.Sprintf("  %s %.1f%%    %s %.1f%%    spread=%.4f\n\n",
				styleYes.Render("YES"), mid*100,
				styleNo.Render("NO"), (1-mid)*100,
				bestAsk-bestBid))
		}

		const maxLevels = 5
		asks := m.ob.asks
		if len(asks) > maxLevels {
			asks = asks[:maxLevels]
		}
		bids := m.ob.bids
		if len(bids) > maxLevels {
			bids = bids[:maxLevels]
		}

		maxQty := 0.0
		for _, l := range append(bids, asks...) {
			if l.qty > maxQty {
				maxQty = l.qty
			}
		}

		sb.WriteString("  " + styleDim.Render("asks") + "\n")
		for i := len(asks) - 1; i >= 0; i-- {
			l := asks[i]
			sb.WriteString(fmt.Sprintf("  %s  $%-6.0f  %s\n",
				styleAsk.Render(fmt.Sprintf("%.4f", l.price)),
				l.qty,
				styleAsk.Render(depthBar(l.qty, maxQty, 20))))
		}
		if len(m.ob.bids) > 0 && len(m.ob.asks) > 0 {
			spread := m.ob.asks[0].price - m.ob.bids[0].price
			sb.WriteString("  " + styleDim.Render(fmt.Sprintf("── spread %.4f ──────────────────────", spread)) + "\n")
		}
		for _, l := range bids {
			sb.WriteString(fmt.Sprintf("  %s  $%-6.0f  %s\n",
				styleBid.Render(fmt.Sprintf("%.4f", l.price)),
				l.qty,
				styleBid.Render(depthBar(l.qty, maxQty, 20))))
		}
		sb.WriteString("  " + styleDim.Render("bids") + "\n")
		if !m.updated.IsZero() {
			sb.WriteString("  " + styleDim.Render("updated "+m.updated.Format("15:04:05.000")) + "\n")
		}
	} else {
		sb.WriteString("  " + styleDim.Render("Connecting to order book...") + "\n")
	}

	sb.WriteString("\n")

	// ── State-specific UI ─────────────────────────────────────────────────────
	switch m.state {

	case stateWatch:
		if m.errMsg != "" {
			sb.WriteString("  " + styleRed.Render(m.errMsg) + "\n\n")
		}
		sb.WriteString("  " + styleDim.Render("y = buy YES   n = buy NO   q = quit") + "\n")

	case stateThreshold:
		var sideLabel, dir string
		currentPrice := 0.0
		if len(m.ob.asks) > 0 {
			currentPrice = m.ob.asks[0].price
		}
		if m.pendingBuy == "yes" {
			sideLabel = styleYes.Render("YES")
			dir = "bid ≥"
		} else {
			sideLabel = styleNo.Render("NO")
			dir = "ask ≤"
		}
		avail := m.available()
		est := 0
		if currentPrice > 0 {
			est = int(avail / currentPrice)
		}
		budgetLine := fmt.Sprintf("$%.2f", avail)
		if m.existing > 0 {
			budgetLine = fmt.Sprintf("$%.2f (budget $%.2f − existing $%.2f)", avail, m.budget, m.existing)
		}
		sb.WriteString(fmt.Sprintf("  Buying %s at %.4f  (%s → ~%d contracts)\n",
			sideLabel, currentPrice, budgetLine, est))
		sb.WriteString(fmt.Sprintf("  Auto-sell when %s price:\n", dir))
		sb.WriteString("  > " + m.threshInput.View() + "\n")
		if m.errMsg != "" {
			sb.WriteString("\n  " + styleRed.Render(m.errMsg) + "\n")
		}
		sb.WriteString("\n  " + styleDim.Render("enter to confirm • esc to cancel") + "\n")

	case stateBuying:
		sb.WriteString("  " + styleWarning.Render("Placing buy order...") + "\n")

	case stateActive:
		if m.pos != nil {
			var sideLabel, dir string
			pnl := 0.0
			if m.pos.intent == "ORDER_INTENT_BUY_LONG" {
				sideLabel = styleYes.Render("LONG YES")
				dir = "bid"
				if len(m.ob.bids) > 0 {
					pnl = (m.ob.bids[0].price - m.pos.entryPrice) * float64(m.pos.qty)
				}
			} else {
				sideLabel = styleNo.Render("SHORT YES (NO)")
				dir = "ask"
				if len(m.ob.asks) > 0 {
					pnl = (m.pos.entryPrice - m.ob.asks[0].price) * float64(m.pos.qty)
				}
			}
			pnlStyle := styleGreen
			if pnl < 0 {
				pnlStyle = styleRed
			}
			sb.WriteString(fmt.Sprintf("  Position: %s  %d contracts @ %.4f\n",
				sideLabel, m.pos.qty, m.pos.entryPrice))
			sb.WriteString(fmt.Sprintf("  Sell when %s ≥ %.4f    P&L: %s\n",
				dir, m.pos.threshold, pnlStyle.Render(fmt.Sprintf("$%.4f", pnl))))
		}
		if m.errMsg != "" {
			sb.WriteString("  " + styleRed.Render(m.errMsg) + "\n")
		}
		sb.WriteString("\n  " + styleDim.Render("Watching threshold... ctrl+c to quit") + "\n")

	case stateSelling:
		sb.WriteString("  " + styleWarning.Render("Executing sell...") + "\n")

	case stateDone:
		pnlStyle := styleGreen
		if m.profit < 0 {
			pnlStyle = styleRed
		}
		sb.WriteString("  " + styleTitle.Render("Trade complete!") + "\n")
		sb.WriteString("  Final P&L: " + pnlStyle.Render(fmt.Sprintf("$%.4f", m.profit)) + "\n\n")
		sb.WriteString("  " + styleDim.Render("Press any key to quit") + "\n")
	}

	// ── Log lines ─────────────────────────────────────────────────────────────
	if len(m.logs) > 0 {
		sb.WriteString("\n")
		for _, l := range m.logs {
			sb.WriteString("  " + styleDim.Render(l) + "\n")
		}
	}

	return sb.String()
}

// ── WebSocket ─────────────────────────────────────────────────────────────────

func parseBook(data []byte, slug string) (snap, bool) {
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
	var bids, asks []level
	for _, e := range frame.MarketData.Bids {
		p, _ := strconv.ParseFloat(e.Px.Value, 64)
		q, _ := strconv.ParseFloat(e.Qty, 64)
		bids = append(bids, level{p, q})
	}
	for _, e := range frame.MarketData.Offers {
		p, _ := strconv.ParseFloat(e.Px.Value, 64)
		q, _ := strconv.ParseFloat(e.Qty, 64)
		asks = append(asks, level{p, q})
	}
	return snap{bids, asks}, true
}

func runWS(slug string, ch chan snap) {
	for {
		if err := connectWS(slug, ch); err != nil {
			time.Sleep(500 * time.Millisecond)
		}
	}
}

func connectWS(slug string, ch chan snap) error {
	ts, sig := sign("GET", "/v1/ws/markets")
	headers := http.Header{
		"X-PM-Access-Key": {keyID},
		"X-PM-Timestamp":  {ts},
		"X-PM-Signature":  {sig},
	}
	dialer := websocket.Dialer{
		ReadBufferSize:   65536,
		HandshakeTimeout: 5 * time.Second,
	}
	conn, _, err := dialer.Dial(wsURL, headers)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetReadLimit(1 << 20)
	conn.WriteJSON(map[string]any{
		"subscribe": map[string]any{
			"requestId":        "manual-1",
			"subscriptionType": "SUBSCRIPTION_TYPE_MARKET_DATA",
			"marketSlugs":      []string{slug},
		},
	})
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		if s, ok := parseBook(data, slug); ok {
			select {
			case ch <- s:
			default: // drop if full, never block the reader
			}
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
	var budget float64
	flag.Float64Var(&budget, "budget", 20.0, "USD budget for this trade")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: manual_bin <market-slug> [--budget N]")
		os.Exit(1)
	}
	slug := flag.Arg(0)

	home, _ := os.UserHomeDir()
	loadEnv(".env")
	loadEnv("../.env")
	loadEnv(home + "/Desktop/Code/modelgopher/modelgopher/.env")

	initAuth()

	existing := existingCommitment(slug)
	if existing > 0 {
		fmt.Printf("[INIT] $%.2f already committed in open orders — available $%.2f of $%.2f\n",
			existing, budget-existing, budget)
	}

	bookCh := make(chan snap, 32)
	go runWS(slug, bookCh)

	m := initialModel(slug, budget, bookCh)
	m.existing = existing
	p, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = p
}
