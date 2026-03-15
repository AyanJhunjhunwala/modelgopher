package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type appState int

const (
	stateSearch appState = iota
	stateList
	stateView
)

type eventsMsg []Event
type balanceMsg string

type refreshMsg struct{ event Event }

var (
	styleTitle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	styleDim       = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	styleCursor    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	styleYesBar    = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	styleNoBar     = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	styleYesLabel  = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)
	styleNoLabel   = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	styleBidBar    = lipgloss.NewStyle().Foreground(lipgloss.Color("45"))
	styleAskBar    = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	styleBotOn     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
)

type model struct {
	state        appState
	input        textinput.Model
	events       []Event
	cursor       int
	listOffset   int
	selected     *Event
	orderBooks   map[string]OrderBook
	updated      time.Time
	scrollOffset int
	height       int
	width        int
	balance      string
	ws           *wsClient
	scalpers     map[string]*os.Process // market slug → running scalper process
}

func initialModel() model {
	ti := textinput.New()
	ti.Placeholder = "e.g. Lakers, Trump, Bitcoin..."
	ti.Focus()
	ti.Width = 50
	return model{
		state:    stateSearch,
		input:    ti,
		height:   24,
		scalpers: make(map[string]*os.Process),
	}
}

func balanceCmd() tea.Cmd {
	return func() tea.Msg {
		bal, _ := fetchBalance()
		return balanceMsg(bal)
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, balanceCmd())
}

func fetchCmd(query string) tea.Cmd {
	return func() tea.Msg {
		events, _ := searchEvents(query)
		return eventsMsg(events)
	}
}

func hotCmd() tea.Cmd {
	return func() tea.Msg {
		events, _ := fetchHotMarkets()
		return eventsMsg(events)
	}
}

func refreshCmd(id string) tea.Cmd {
	return func() tea.Msg {
		e, err := fetchEvent(id)
		if err != nil || e == nil {
			return nil
		}
		return refreshMsg{event: *e}
	}
}

// scalperBinPath finds the scalper binary by checking several locations.
func scalperBinPath() string {
	home, _ := os.UserHomeDir()
	candidates := []string{
		// absolute project path (most reliable)
		filepath.Join(home, "Desktop", "Code", "modelgopher", "scalper", "scalper_bin"),
		// relative to cwd (when running the built binary)
		"../scalper/scalper_bin",
		"./scalper/scalper_bin",
	}
	for _, p := range candidates {
		if abs, err := filepath.Abs(p); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return abs
			}
		}
	}
	return candidates[0] // fall back to absolute even if not yet built
}

// launchScalper opens a new Terminal window running the scalper for slug.
func launchScalper(slug string) (*os.Process, error) {
	bin := scalperBinPath()
	dir := filepath.Dir(bin)
	script := fmt.Sprintf(
		`tell application "Terminal" to do script "cd '%s' && ./scalper_bin '%s'"`,
		dir, slug,
	)
	cmd := exec.Command("osascript", "-e", script)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Process, nil
}

// listViewHeight returns the number of rows available for list items.
func (m model) listViewHeight() int {
	h := m.height - 4
	if h < 1 {
		h = 1
	}
	return h
}

// clampListOffset keeps the cursor visible in the list viewport.
func (m *model) clampListOffset() {
	vh := m.listViewHeight()
	if m.cursor < m.listOffset {
		m.listOffset = m.cursor
	}
	if m.cursor >= m.listOffset+vh {
		m.listOffset = m.cursor - vh + 1
	}
	m.listOffset = max(m.listOffset, 0)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.height = msg.Height
		m.width = msg.Width
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			m.ws.close()
			for _, p := range m.scalpers {
				p.Kill()
			}
			return m, tea.Quit

		case "tab":
			if m.state == stateSearch {
				return m, hotCmd()
			}

		case "esc":
			if m.state == stateView {
				m.ws.close()
				m.ws = nil
				m.state = stateList
				m.selected = nil
				m.orderBooks = nil
				m.scrollOffset = 0
			} else if m.state == stateList {
				m.state = stateSearch
				m.events = nil
				m.cursor = 0
				m.listOffset = 0
			}
			return m, nil

		case "s":
			if m.state == stateView && m.selected != nil {
				slug := m.selected.ID
				if proc, ok := m.scalpers[slug]; ok {
					proc.Kill()
					delete(m.scalpers, slug)
				} else {
					if proc, err := launchScalper(slug); err == nil {
						m.scalpers[slug] = proc
					}
				}
			}
			return m, nil

		case "enter":
			if m.state == stateSearch {
				return m, fetchCmd(m.input.Value())
			}
			if m.state == stateList && len(m.events) > 0 {
				e := m.events[m.cursor]
				m.selected = &e
				m.state = stateView
				m.updated = time.Now()
				m.scrollOffset = 0
				m.ws.close()
				m.ws = nil
				return m, refreshCmd(e.ID)
			}

		case "up", "k":
			if m.state == stateList && m.cursor > 0 {
				m.cursor--
				m.clampListOffset()
			} else if m.state == stateView && m.scrollOffset > 0 {
				m.scrollOffset--
			}

		case "down", "j":
			if m.state == stateList && m.cursor < len(m.events)-1 {
				m.cursor++
				m.clampListOffset()
			} else if m.state == stateView {
				m.scrollOffset++
			}
		}

	case refreshMsg:
		m.selected = &msg.event
		m.updated = time.Now()
		return m, wsConnectCmd(msg.event.ID)

	case wsConnectedMsg:
		m.ws = msg.client
		return m, wsWaitCmd(m.ws)

	case wsBookMsg:
		if m.orderBooks == nil {
			m.orderBooks = make(map[string]OrderBook)
		}
		m.orderBooks[msg.slug] = msg.ob
		m.updated = time.Now()
		return m, wsWaitCmd(m.ws)

	case wsErrMsg:
		m.ws = nil
		if m.state == stateView && m.selected != nil {
			return m, wsConnectCmd(m.selected.ID)
		}
		return m, nil

	case eventsMsg:
		m.events = []Event(msg)
		m.cursor = 0
		m.listOffset = 0
		if len(m.events) == 0 {
			m.state = stateSearch
		} else {
			m.state = stateList
		}
		return m, nil

	case balanceMsg:
		m.balance = string(msg)
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// topN returns at most n order book entries.
func topN(entries []OrderEntry, n int) []OrderEntry {
	if len(entries) <= n {
		return entries
	}
	return entries[:n]
}

// headerLine right-aligns badges (balance, bot status) against window width.
func (m model) headerLine(left string) string {
	var badges []string
	if m.state == stateView && m.selected != nil {
		if _, ok := m.scalpers[m.selected.ID]; ok {
			badges = append(badges, styleBotOn.Render("[BOT ON]"))
		}
	}
	if m.balance != "" {
		badges = append(badges, styleDim.Render("$"+m.balance))
	}
	if len(badges) == 0 || m.width == 0 {
		return left
	}
	right := strings.Join(badges, "  ")
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

func depthBar(value, maxVal float64, width int, style lipgloss.Style) string {
	if maxVal == 0 {
		return ""
	}
	n := int((value / maxVal) * float64(width))
	if n < 0 {
		n = 0
	}
	if n > width {
		n = width
	}
	return style.Render(strings.Repeat("█", n))
}

func (m model) View() string {
	switch m.state {
	case stateSearch:
		return fmt.Sprintf(
			"\n  %s\n\n  %s\n\n  %s\n",
			m.headerLine("  "+styleTitle.Render("Polymarket US")),
			m.input.View(),
			styleDim.Render("enter to search • tab for hot markets • ctrl+c to quit"),
		)

	case stateList:
		if len(m.events) == 0 {
			return fmt.Sprintf("\n  %s\n\n  %s\n",
				"No markets found.",
				styleDim.Render("esc to search again"),
			)
		}
		var sb strings.Builder
		sb.WriteString("\n" + m.headerLine("  "+styleTitle.Render("Markets")) + "\n\n")
		vh := m.listViewHeight()
		end := min(m.listOffset+vh, len(m.events))
		for i := m.listOffset; i < end; i++ {
			e := m.events[i]
			botTag := ""
			if _, ok := m.scalpers[e.ID]; ok {
				botTag = " " + styleBotOn.Render("●")
			}
			if i == m.cursor {
				sb.WriteString(fmt.Sprintf("  %s %s%s\n",
					styleCursor.Render(">"), styleCursor.Render(e.Title), botTag))
			} else {
				sb.WriteString(fmt.Sprintf("    %s%s\n", e.Title, botTag))
			}
		}
		scrollInfo := ""
		if len(m.events) > vh {
			scrollInfo = styleDim.Render(fmt.Sprintf(" (%d/%d)", m.cursor+1, len(m.events)))
		}
		fmt.Fprintf(&sb, "\n  %s%s\n", styleDim.Render("↑/↓ navigate • enter select • esc back"), scrollInfo)
		return sb.String()

	case stateView:
		if m.selected == nil {
			return ""
		}
		e := m.selected
		var lines []string
		lines = append(lines, "")
		lines = append(lines, m.headerLine("  "+styleTitle.Render(e.Title)))
		lines = append(lines, "  "+styleDim.Render("updated "+m.updated.Format("15:04:05")))
		lines = append(lines, "")

		for _, mkt := range e.Markets {
			if mkt.Question != "" {
				lines = append(lines, "  "+styleDim.Render(mkt.Question))
			}

			var yesSides, noSides []MarketSide
			for _, s := range mkt.MarketSides {
				if s.Long {
					yesSides = append(yesSides, s)
				} else {
					noSides = append(noSides, s)
				}
			}

			type displayItem struct {
				name  string
				price float64
				yes   bool
			}
			var items []displayItem
			if len(yesSides) == 1 && len(noSides) == 1 {
				yp, _ := strconv.ParseFloat(yesSides[0].Price, 64)
				items = append(items, displayItem{yesSides[0].Description, yp, true})
				items = append(items, displayItem{noSides[0].Description, 1.0 - yp, false})
			} else {
				for i, s := range yesSides {
					p, _ := strconv.ParseFloat(s.Price, 64)
					items = append(items, displayItem{s.Description, p, i == 0})
				}
			}

			if len(items) == 0 {
				continue
			}

			for _, item := range items {
				barLen := int(item.price * 40)
				if barLen < 0 {
					barLen = 0
				}
				if barLen > 40 {
					barLen = 40
				}
				bar := strings.Repeat("█", barLen)
				pct := fmt.Sprintf("%5.1f%%", item.price*100)
				label := fmt.Sprintf("%-12s", item.name)
				if len(label) > 12 {
					label = item.name[:12]
				}
				if item.yes {
					lines = append(lines, fmt.Sprintf("  %s %s  %s",
						styleYesLabel.Render(label), pct, styleYesBar.Render(bar)))
				} else {
					lines = append(lines, fmt.Sprintf("  %s %s  %s",
						styleNoLabel.Render(label), pct, styleNoBar.Render(bar)))
				}
			}

			if mkt.Slug != "" && m.orderBooks != nil {
				if ob, ok := m.orderBooks[mkt.Slug]; ok {
					const bookLevels = 7
					bids := topN(ob.Bids, bookLevels)
					asks := topN(ob.Asks, bookLevels)

					maxQty := 0.0
					for _, e := range append(bids, asks...) {
						q, _ := strconv.ParseFloat(e.Size, 64)
						if q > maxQty {
							maxQty = q
						}
					}

					lines = append(lines, "  "+styleDim.Render("┌── asks ───────────────────────"))
					for i := len(asks) - 1; i >= 0; i-- {
						e := asks[i]
						qty, _ := strconv.ParseFloat(e.Size, 64)
						lines = append(lines, fmt.Sprintf("  %s  %s  %s",
							styleAskBar.Render(fmt.Sprintf("%-7s", e.Price)),
							styleDim.Render(fmt.Sprintf("$%-7.0f", qty)),
							depthBar(qty, maxQty, 22, styleAskBar)))
					}

					if len(bids) > 0 && len(asks) > 0 {
						bestBid, _ := strconv.ParseFloat(bids[0].Price, 64)
						bestAsk, _ := strconv.ParseFloat(asks[0].Price, 64)
						spread := bestAsk - bestBid
						lines = append(lines, fmt.Sprintf("  %s",
							styleDim.Render(fmt.Sprintf("├── spread %-6.4f ────────────────", spread))))
					}

					for _, e := range bids {
						qty, _ := strconv.ParseFloat(e.Size, 64)
						lines = append(lines, fmt.Sprintf("  %s  %s  %s",
							styleBidBar.Render(fmt.Sprintf("%-7s", e.Price)),
							styleDim.Render(fmt.Sprintf("$%-7.0f", qty)),
							depthBar(qty, maxQty, 22, styleBidBar)))
					}
					lines = append(lines, "  "+styleDim.Render("└── bids ───────────────────────"))
				}
			}

			lines = append(lines, "")
		}

		footer := "  " + styleDim.Render("↑/↓ scroll • s scalper • esc back • ctrl+c quit")
		viewHeight := m.height - 1
		maxScroll := max(len(lines)-viewHeight, 0)
		if m.scrollOffset > maxScroll {
			m.scrollOffset = maxScroll
		}
		end := m.scrollOffset + viewHeight
		if end > len(lines) {
			end = len(lines)
		}
		visible := lines[m.scrollOffset:end]
		indicator := ""
		if len(lines) > viewHeight {
			indicator = styleDim.Render(fmt.Sprintf(" (%d/%d)", m.scrollOffset+viewHeight, len(lines)))
		}
		return strings.Join(visible, "\n") + "\n" + footer + indicator + "\n"
	}
	return ""
}
