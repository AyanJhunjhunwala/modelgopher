package main

import (
    "bufio"
    "fmt"
    "log"
    "os"
    "strings"
)
func main() {
	reader := bufio.NewReader(os.Stdin)

	fmt.Print("Search markets: ")
	query, _ := reader.ReadString('\n')
	query = strings.TrimSpace(query)

    events, err := searchEvents(query)
    if err != nil {
        log.Fatal(err)
    }

	if len(events) == 0 {
        fmt.Println("No markets found.")
        return
    }
	renderVolume(events)
}
