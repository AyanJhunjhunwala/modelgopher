package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

type Event struct {
	Title     string  `json:"title"`
	Volume24h float64 `json:"volume24hr"`
	Liquidity float64 `json:"liquidity"`
}

func fetchEvents() ([]Event, error) {
	resp, err := http.Get(BaseURL + "/events?active=true&closed=false&limit=10")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var events []Event
	if err := json.Unmarshal(body, &events); err != nil {
		return nil, err
	}
	return events, nil
}


func searchEvents(query string) ([]Event, error) {
	resp, err := http.Get(BaseURL + "/events?active=true&closed=false&limit=100&&tag_id=745")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var events []Event
	if err := json.Unmarshal(body, &events); err != nil {
		return nil, err
	}

	lower := strings.ToLower(query)
	var matched []Event
	for _, e := range events {
		if strings.Contains(strings.ToLower(e.Title), lower) {
			matched = append(matched, e)
		}
	}
	return matched, nil
}
