package main

import (
	"encoding/json"
	"fmt"
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
type refreshMsg Event

var (
	styleTitle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	styleDim      = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	styleCursor   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	styleYesBar   = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	styleNoBar    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	styleYesLabel = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)
	styleNoLabel  = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
)

type model struct {
	state        appState
	input        textinput.Model
	events       []Event
	cursor       int
	selected     *Event
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
		return refreshMsg(*e)
	}
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
		case "esc":
			if m.state == stateView {
				m.state = stateList
				m.selected = nil
				m.scrollOffset = 0
			} else if m.state == stateList {
				m.state = stateSearch
				m.events = nil
				m.cursor = 0
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
				return m, tickCmd()
			}
		case "up", "k":
			if m.state == stateList && m.cursor > 0 {
				m.cursor--
			} else if m.state == stateView && m.scrollOffset > 0 {
				m.scrollOffset--
			}
		case "down", "j":
			if m.state == stateList && m.cursor < len(m.events)-1 {
				m.cursor++
			} else if m.state == stateView {
				m.scrollOffset++
			}
		}

	case refreshMsg:
		ev := Event(msg)
		m.selected = &ev
		m.updated = time.Now()
		return m, tickCmd()

	case eventsMsg:
		if m.state == stateView {
			return m, tickCmd()
		}
		m.events = []Event(msg)
		if len(m.events) == 0 {
			m.state = stateSearch
		} else {
			m.state = stateList
			m.cursor = 0
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

func (m model) View() string {
	switch m.state {
	case stateSearch:
		return fmt.Sprintf(
			"\n  %s\n\n  %s\n\n  %s\n",
			styleTitle.Render("Polymarket CLI"),
			m.input.View(),
			styleDim.Render("enter to search • ctrl+c to quit"),
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
		for i, e := range m.events {
			if i == m.cursor {
				sb.WriteString(fmt.Sprintf("  %s %s\n", styleCursor.Render(">"), styleCursor.Render(e.Title)))
			} else {
				sb.WriteString(fmt.Sprintf("    %s\n", e.Title))
			}
		}
		sb.WriteString(fmt.Sprintf("\n  %s\n", styleDim.Render("↑/↓ navigate • enter select • esc back")))
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
			json.Unmarshal([]byte(mkt.Outcomes), &outcomes)
			json.Unmarshal([]byte(mkt.OutcomePrices), &prices)
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
			lines = append(lines, "  "+styleDim.Render(fmt.Sprintf("vol: $%.2f", mkt.Volume)))
			lines = append(lines, "")
		}

		// footer always visible — reserve 1 line
		footer := "  " + styleDim.Render("↑/↓ scroll • esc back • ctrl+c quit")
		viewHeight := m.height - 1

		// clamp scroll
		maxScroll := len(lines) - viewHeight
		if maxScroll < 0 {
			maxScroll = 0
		}
		if m.scrollOffset > maxScroll {
			m.scrollOffset = maxScroll
		}

		end := m.scrollOffset + viewHeight
		if end > len(lines) {
			end = len(lines)
		}
		visible := lines[m.scrollOffset:end]

		// scroll indicator
		indicator := ""
		if len(lines) > viewHeight {
			indicator = styleDim.Render(fmt.Sprintf(" (%d/%d)", m.scrollOffset+viewHeight, len(lines)))
		}

		return strings.Join(visible, "\n") + "\n" + footer + indicator + "\n"
	}
	return ""
}
