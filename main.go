package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	reader := bufio.NewReader(os.Stdin)

	fmt.Print("Search markets: ")
	query, _ := reader.ReadString('\n')
	query = strings.TrimSpace(query)

	// handle Ctrl+C gracefully
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	refresh(query)

	for {
		select {
		case <-ticker.C:
			refresh(query)
		case <-quit:
			fmt.Println("\nExiting.")
			return
		}
	}
}

func refresh(query string) {
	events, err := searchEvents(query)
	if err != nil {
		log.Println("fetch error:", err)
		return
	}
	if len(events) == 0 {
		fmt.Println("No markets found.")
		return
	}
	fmt.Print("\033[H\033[2J") // clear terminal
	fmt.Printf("Live volume — updated %s (Ctrl+C to quit)\n\n", time.Now().Format("15:04:05"))
	renderVolume(events)
}
