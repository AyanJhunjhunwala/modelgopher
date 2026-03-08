package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

type Market struct {
	Question      string  `json:"question"`
	Outcomes      string  `json:"outcomes"`
	OutcomePrices string  `json:"outcomePrices"`
	Volume        float64 `json:"volumeNum"`
}

type Event struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Volume24h float64  `json:"volume24hr"`
	Liquidity float64  `json:"liquidity"`
	Markets   []Market `json:"markets"`
}

func fetchEvent(id string) (*Event, error) {
	resp, err := http.Get(BaseURL + "/events/" + id)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var e Event
	if err := json.Unmarshal(body, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

func fetchPage(offset int) ([]Event, error) {
	url := BaseURL + fmt.Sprintf("/events?active=true&closed=false&limit=500&offset=%d", offset)
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var events []Event
	json.Unmarshal(body, &events)
	return events, nil
}

func searchEvents(query string) ([]Event, error) {
	const pageSize = 500

	// probe pages concurrently in batches until we get an empty page
	var allEvents []Event
	var mu sync.Mutex

	for batchStart := 0; ; batchStart += 10 {
		type result struct {
			page   int
			events []Event
		}
		results := make([]result, 10)
		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				offset := (batchStart + i) * pageSize
				events, _ := fetchPage(offset)
				results[i] = result{page: batchStart + i, events: events}
			}(i)
		}
		wg.Wait()

		anyNonEmpty := false
		for _, r := range results {
			if len(r.events) > 0 {
				anyNonEmpty = true
				mu.Lock()
				allEvents = append(allEvents, r.events...)
				mu.Unlock()
			}
		}
		if !anyNonEmpty {
			break
		}
	}

	lower := strings.ToLower(query)
	seen := make(map[string]bool)
	var matched []Event
	for _, e := range allEvents {
		if !seen[e.Title] && strings.Contains(strings.ToLower(e.Title), lower) {
			seen[e.Title] = true
			matched = append(matched, e)
		}
	}
	return matched, nil
}
