package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/gorilla/websocket"
	tea "github.com/charmbracelet/bubbletea"
)

const wsMarketsURL = "wss://api.polymarket.us/v1/ws/markets"

// wsClient owns the connection and the channel that the persistent reader
// goroutine writes into. One goroutine per connection, running for its lifetime.
type wsClient struct {
	conn *websocket.Conn
	slug string
	msgs chan tea.Msg // buffered; reader goroutine writes, Tea cmd reads
}

func (c *wsClient) close() {
	if c != nil && c.conn != nil {
		c.conn.Close() // unblocks ReadMessage in the reader goroutine
	}
}

// Bubble Tea message types.
type wsConnectedMsg struct{ client *wsClient }
type wsBookMsg struct {
	slug string
	ob   OrderBook
}
type wsErrMsg struct{ err error }

// wsConnectCmd dials the WebSocket, subscribes, and spawns the persistent
// reader goroutine. Returns wsConnectedMsg so the Update handler can start
// draining the channel via wsWaitCmd.
func wsConnectCmd(slug string) tea.Cmd {
	return func() tea.Msg {
		path := "/v1/ws/markets"
		ts, sig, err := signRequest("GET", path)
		if err != nil {
			return wsErrMsg{fmt.Errorf("ws sign: %w", err)}
		}

		headers := http.Header{}
		headers.Set("X-PM-Access-Key", os.Getenv("PM_KEY_ID"))
		headers.Set("X-PM-Timestamp", ts)
		headers.Set("X-PM-Signature", sig)

		conn, _, err := websocket.DefaultDialer.Dial(wsMarketsURL, headers)
		if err != nil {
			return wsErrMsg{fmt.Errorf("ws dial: %w", err)}
		}

		sub := map[string]any{
			"subscribe": map[string]any{
				"requestId":        "book-1",
				"subscriptionType": "SUBSCRIPTION_TYPE_MARKET_DATA",
				"marketSlugs":      []string{slug},
			},
		}
		if err := conn.WriteJSON(sub); err != nil {
			conn.Close()
			return wsErrMsg{fmt.Errorf("ws subscribe: %w", err)}
		}

		c := &wsClient{
			conn: conn,
			slug: slug,
			msgs: make(chan tea.Msg, 64), // buffer absorbs bursts during Tea processing
		}

		// Persistent reader goroutine: runs for the lifetime of the connection.
		// Decoupled from the Bubble Tea event loop so reading never stalls.
		go func() {
			for {
				_, data, err := conn.ReadMessage()
				if err != nil {
					c.msgs <- wsErrMsg{err}
					return
				}
				if msg := parseBookFrame(data); msg != nil {
					c.msgs <- msg
				}
				// Heartbeats / unknown frames: parseBookFrame returns nil, keep reading.
			}
		}()

		return wsConnectedMsg{c}
	}
}

// wsWaitCmd blocks on the channel until the reader goroutine delivers a msg.
// Re-dispatched by the Update handler after every message.
func wsWaitCmd(c *wsClient) tea.Cmd {
	return func() tea.Msg {
		return <-c.msgs
	}
}

// parseBookFrame parses a raw WebSocket frame into a wsBookMsg.
// Returns nil for heartbeats, non-book frames, or parse errors.
func parseBookFrame(data []byte) tea.Msg {
	var frame struct {
		MarketData struct {
			MarketSlug string      `json:"marketSlug"`
			Bids       []bookEntry `json:"bids"`
			Offers     []bookEntry `json:"offers"`
		} `json:"marketData"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		return nil
	}
	if frame.MarketData.MarketSlug == "" {
		return nil
	}

	ob := OrderBook{}
	for _, e := range frame.MarketData.Bids {
		ob.Bids = append(ob.Bids, OrderEntry{Price: e.Px.Value, Size: e.Qty})
	}
	for _, e := range frame.MarketData.Offers {
		ob.Asks = append(ob.Asks, OrderEntry{Price: e.Px.Value, Size: e.Qty})
	}
	return wsBookMsg{slug: frame.MarketData.MarketSlug, ob: ob}
}
