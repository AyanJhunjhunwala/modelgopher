package main

import "fmt"

func renderVolume(events []Event) {
	for _, e := range events {
		fmt.Printf("%s — 24h vol: $%.2f\n", e.Title, e.Volume24h)
	}
}
