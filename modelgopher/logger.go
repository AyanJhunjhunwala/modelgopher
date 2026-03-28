package main

// ofiLogger writes OFI snapshots to a CSV file.
// Toggle with the 'l' key while viewing a market.
// File is created in the current working directory with name:
//   ofi_<slug>_<YYYYMMDD_HHMMSS>.csv

import (
	"fmt"
	"os"
	"sync"
	"time"
)

type ofiLogger struct {
	mu     sync.Mutex
	file   *os.File
	active bool
	path   string
}

var ofiLog ofiLogger

// toggle starts logging if inactive, stops it if active.
// Returns (nowActive, filename, error).
func (l *ofiLogger) toggle(slug string) (bool, string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active {
		l.stopLocked()
		return false, "", nil
	}
	fname := fmt.Sprintf("ofi_%s_%s.csv", slug, time.Now().Format("20060102_150405"))
	f, err := os.Create(fname)
	if err != nil {
		return false, "", err
	}
	fmt.Fprintf(f, "timestamp_ms,slug,ofi_raw,ofi_norm,best_bid,best_ask,spread\n")
	l.file = f
	l.path = fname
	l.active = true
	return true, fname, nil
}

// write appends one row. No-op if logging is not active.
func (l *ofiLogger) write(slug string, ofiRaw, ofiNorm, bestBid, bestAsk, spread float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.active || l.file == nil {
		return
	}
	fmt.Fprintf(l.file, "%d,%s,%.6f,%.6f,%.4f,%.4f,%.4f\n",
		time.Now().UnixMilli(), slug, ofiRaw, ofiNorm, bestBid, bestAsk, spread)
}

// stop closes the log file.
func (l *ofiLogger) stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stopLocked()
}

func (l *ofiLogger) stopLocked() {
	if l.file != nil {
		l.file.Close()
		l.file = nil
	}
	l.active = false
	l.path = ""
}

func (l *ofiLogger) isActive() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active
}
