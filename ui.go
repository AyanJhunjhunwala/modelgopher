package main

import (
	"encoding/json"
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
}

func initialModel() model {
	ti := textinput.New()
	ti.Placeholder = "e.g. Lakers, Trump, Bitcoin..."
	ti.Focus()
	ti.Width = 50
	return model{state: stateSearch, input: ti, height: 24}
}

func (m model) Init() tea.Cmd {
	return textinput.Blink
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

func sumSizes(entries []OrderEntry) float64 {
	var total float64
	for _, e := range entries {
		v, _ := strconv.ParseFloat(e.Size, 64)
		total += v
	}
	return total
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
			styleTitle.Render("Polymarket CLI"),
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
		sb.WriteString(fmt.Sprintf("\n  %s\n\n", styleTitle.Render("Select a market")))

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
		lines = append(lines, "  "+styleTitle.Render(e.Title))
		lines = append(lines, fmt.Sprintf("  24h vol: $%-12.2f  liquidity: $%.2f", e.Volume24h, e.Liquidity))
		lines = append(lines, "  "+styleDim.Render("updated "+m.updated.Format("15:04:05")))
		lines = append(lines, "")

		for _, mkt := range e.Markets {
			var outcomes []string
			var prices []string
			var tokenIDs []string
			json.Unmarshal([]byte(mkt.Outcomes), &outcomes)
			json.Unmarshal([]byte(mkt.OutcomePrices), &prices)
			json.Unmarshal([]byte(mkt.ClobTokenIds), &tokenIDs)
			if len(outcomes) < 2 || len(prices) < 2 {
				continue
			}

			if mkt.Question != "" {
				lines = append(lines, "  "+styleDim.Render(mkt.Question))
			}

			for i, outcome := range outcomes {
				var price float64
				fmt.Sscanf(prices[i], "%f", &price)
				barLen := int(price * 40)
				if barLen < 0 {
					barLen = 0
				}
				bar := strings.Repeat("█", barLen)
				pct := fmt.Sprintf("%5.1f%%", price*100)
				if i == 0 {
					lines = append(lines, fmt.Sprintf("  %s %s  %s",
						styleYesLabel.Render(fmt.Sprintf("%-4s", outcome)), pct, styleYesBar.Render(bar)))
				} else {
					lines = append(lines, fmt.Sprintf("  %s %s  %s",
						styleNoLabel.Render(fmt.Sprintf("%-4s", outcome)), pct, styleNoBar.Render(bar)))
				}
			}

			if len(tokenIDs) > 0 && m.orderBooks != nil {
				if ob, ok := m.orderBooks[tokenIDs[0]]; ok {
					bidTotal := sumSizes(ob.Bids)
					askTotal := sumSizes(ob.Asks)
					maxDepth := bidTotal
					if askTotal > maxDepth {
						maxDepth = askTotal
					}
					lines = append(lines, fmt.Sprintf("  %s $%-10.0f  %s",
						styleBidBar.Render("BID"), bidTotal,
						depthBar(bidTotal, maxDepth, 30, styleBidBar)))
					lines = append(lines, fmt.Sprintf("  %s $%-10.0f  %s",
						styleAskBar.Render("ASK"), askTotal,
						depthBar(askTotal, maxDepth, 30, styleAskBar)))
				}
			}

			lines = append(lines, "  "+styleDim.Render(fmt.Sprintf("vol: $%.2f", mkt.Volume)))
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
