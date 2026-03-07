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

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)

	for {
		fmt.Print("Search markets: ")
		query, _ := reader.ReadString('\n')
		query = strings.TrimSpace(query)

		events, err := searchEvents(query)
		if err != nil {
			log.Println("fetch error:", err)
			continue
		}
		if len(events) == 0 {
			fmt.Println("No markets found. Try again.\n")
			continue
		}

		// live update loop — blocks until Ctrl+C
		ticker := time.NewTicker(1 * time.Second)
		exited := false
		lines := 0
		for !exited {
			if lines > 0 {
				fmt.Printf("\033[%dA", lines) // move cursor up
			}
			header := fmt.Sprintf("Live volume — updated %s (Ctrl+C to quit)\n\n", time.Now().Format("15:04:05"))
			fmt.Print(header)
			lines = 2 + renderVolume(events)

			select {
			case <-ticker.C:
				updated, err := searchEvents(query)
				if err == nil && len(updated) > 0 {
					events = updated
				}
			case <-quit:
				ticker.Stop()
				fmt.Println("\nExiting.")
				exited = true
			}
		}
		if exited {
			return
		}
	}
}
