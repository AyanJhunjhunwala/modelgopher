package main


import (
    "encoding/json"
    "fmt"
    "io"
    "log"
    "net/http"
)


type Event struct {
    Title     string  `json:"title"`
    Volume24h float64 `json:"volume24hr"`
    Liquidity float64 `json:"liquidity"`
}

func main(){
	resp, err := http.Get("https://gamma-api.polymarket.com/events?active=true&closed=false&limit=10")
	if err != nil {
		log.Fatal(err)
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
        log.Fatal(err)
    }
	var events []Event
    if err := json.Unmarshal(body, &events); err != nil {
        log.Fatal(err)
    }

    for _, e := range events {
        fmt.Printf("%s — 24h vol: $%.2f\n", e.Title, e.Volume24h)
    }
}


// GET https://gamma-api.polymarket.com/events?active=true&closed=false&limit=10
