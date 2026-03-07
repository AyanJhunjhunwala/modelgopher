package main

import "log"

func main() {
	events, err := fetchEvents()
	if err != nil {
		log.Fatal(err)
	}
	renderVolume(events)
}
