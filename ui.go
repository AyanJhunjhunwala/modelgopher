package main

import (
	"fmt"
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
type tickMsg time.Time
type balanceMsg string

type refreshMsg struct {
	event      Event
	orderBooks map[string]OrderBook
}

var (
	styleTitle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	styleDim      = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	styleCursor   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	styleYesBar   = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	styleNoBar    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	styleYesLabel = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)
	styleNoLabel  = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	styleBidBar = lipgloss.NewStyle().Foreground(lipgloss.Color("45"))
	styleAskBar = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
)

type model struct {
	state        appState
	input        textinput.Model
	events       []Event
	cursor       int
	listOffset   int // first visible row in the list
	selected     *Event
	orderBooks   map[string]OrderBook
	updated      time.Time
	scrollOffset int
	height       int
	width        int
	balance      string
}

func initialModel() model {
	ti := textinput.New()
	ti.Placeholder = "e.g. Lakers, Trump, Bitcoin..."
	ti.Focus()
	ti.Width = 50
	return model{state: stateSearch, input: ti, height: 24}
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

func tickCmd() tea.Cmd {
	return tea.Tick(1*time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func refreshCmd(id string) tea.Cmd {
	return func() tea.Msg {
		e, err := fetchEvent(id)
		if err != nil || e == nil {
			return nil
		}
		books := fetchOrderBooks(e)
		return refreshMsg{event: *e, orderBooks: books}
	}
}

// listViewHeight returns the number of rows available for list items.
func (m model) listViewHeight() int {
	// header (2) + footer (2) = 4 reserved lines
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
			return m, tea.Quit

		case "tab":
			if m.state == stateSearch {
				return m, hotCmd()
			}

		case "esc":
			if m.state == stateView {
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
		m.orderBooks = msg.orderBooks
		m.updated = time.Now()
		return m, tickCmd()

	case eventsMsg:
		if m.state == stateView {
			return m, tickCmd()
		}
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

	case tickMsg:
		if m.state == stateView && m.selected != nil {
			return m, refreshCmd(m.selected.ID)
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// topN returns at most n order book entries (API returns best-first).
func topN(entries []OrderEntry, n int) []OrderEntry {
	if len(entries) <= n {
		return entries
	}
	return entries[:n]
}

func sumSizes(entries []OrderEntry) float64 {
	var total float64
	for _, e := range entries {
		v, _ := strconv.ParseFloat(e.Size, 64)
		total += v
	}
	return total
}

// headerLine right-aligns the balance badge against the window width.
func (m model) headerLine(left string) string {
	if m.balance == "" || m.width == 0 {
		return left
	}
	badge := styleDim.Render("$" + m.balance)
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(badge) - 2
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + badge
}

func depthBar(value, max float64, width int, style lipgloss.Style) string {
	if max == 0 {
		return ""
	}
	barLen := int((value / max) * float64(width))
	if barLen < 0 {
		barLen = 0
	}
	if barLen > width {
		barLen = width
	}
	return style.Render(strings.Repeat("█", barLen))
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
			if i == m.cursor {
				sb.WriteString(fmt.Sprintf("  %s %s\n", styleCursor.Render(">"), styleCursor.Render(e.Title)))
			} else {
				sb.WriteString(fmt.Sprintf("    %s\n", e.Title))
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

			// Separate long (YES) and short (NO) sides.
			// long=true sides always carry a proper 0–1 probability.
			// long=false sides store decimal odds, not probabilities.
			var yesSides, noSides []MarketSide
			for _, s := range mkt.MarketSides {
				if s.Long {
					yesSides = append(yesSides, s)
				} else {
					noSides = append(noSides, s)
				}
			}

			// Build display items: for a binary market (1 YES + 1 NO) show both.
			// For multi-outcome markets show all YES sides.
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
				label := fmt.Sprintf("%-6s", item.name)
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
					bids := topN(ob.Bids, bookLevels) // descending: best bid first
					asks := topN(ob.Asks, bookLevels) // ascending:  best ask first

					// Compute max qty across all visible levels for bar scaling.
					maxQty := 0.0
					for _, e := range append(bids, asks...) {
						q, _ := strconv.ParseFloat(e.Size, 64)
						if q > maxQty {
							maxQty = q
						}
					}

					// Asks: display reversed so the best ask is closest to the spread.
					lines = append(lines, "  "+styleDim.Render("┌── asks ───────────────────────"))
					for i := len(asks) - 1; i >= 0; i-- {
						e := asks[i]
						qty, _ := strconv.ParseFloat(e.Size, 64)
						lines = append(lines, fmt.Sprintf("  %s  %s  %s",
							styleAskBar.Render(fmt.Sprintf("%-7s", e.Price)),
							styleDim.Render(fmt.Sprintf("$%-7.0f", qty)),
							depthBar(qty, maxQty, 22, styleAskBar)))
					}

					// Spread line.
					if len(bids) > 0 && len(asks) > 0 {
						bestBid, _ := strconv.ParseFloat(bids[0].Price, 64)
						bestAsk, _ := strconv.ParseFloat(asks[0].Price, 64)
						spread := bestAsk - bestBid
						lines = append(lines, fmt.Sprintf("  %s",
							styleDim.Render(fmt.Sprintf("├── spread %-6.4f ────────────────", spread))))
					}

					// Bids: best bid (highest price) at top, descending.
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

		footer := "  " + styleDim.Render("↑/↓ scroll • esc back • ctrl+c quit")
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
