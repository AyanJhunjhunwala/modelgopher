package main

import (
	"bufio"
	"os"
	"strings"
)

const (
	GatewayURL = "https://gateway.polymarket.us"
	AccountURL = "https://api.polymarket.us"
)

// loadEnv reads a .env file and sets environment variables.
func loadEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		os.Setenv(strings.TrimSpace(k), strings.TrimSpace(v))
	}
}
