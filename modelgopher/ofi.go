package main

// OFI (Order Flow Imbalance) — Cont, Kukanov & Stoikov (2014).
//
// On each book update we compare the new best bid/ask to the previous
// snapshot and compute the net order-flow contribution at the touch:
//
//   Bid contribution:
//     bid_price >  prev  →  +bid_qty        (new higher bid arrived)
//     bid_price == prev  →  +(bid_qty - prev_bid_qty)
//     bid_price <  prev  →  -prev_bid_qty   (best bid was lifted / cancelled)
//
//   Ask contribution (ask depth decrease = buy pressure):
//     ask_price <  prev  →  -ask_qty        (new lower ask arrived)
//     ask_price == prev  →  +(prev_ask_qty - ask_qty)
//     ask_price >  prev  →  +prev_ask_qty   (best ask was lifted)
//
//   OFI(t) = bid_contribution + ask_contribution
//
// Positive OFI → net buy pressure. Negative → net sell pressure.

import (
	"math"
	"strconv"
	"sync"
)

const ofiWindow = 30 // rolling window size for normalization

type ofiState struct {
	mu       sync.Mutex
	prevBid  float64
	prevAsk  float64
	prevBidQ float64
	prevAskQ float64
	ring     [ofiWindow]float64 // circular buffer of per-tick OFI values
	head     int                // next write index
	count    int                // total ticks seen (including first seed)
}

var ofiMap sync.Map // slug → *ofiState

func getOFI(slug string) *ofiState {
	v, _ := ofiMap.LoadOrStore(slug, &ofiState{})
	return v.(*ofiState)
}

func resetOFIState(slug string) {
	ofiMap.Delete(slug)
}

// update computes OFI from a new book snapshot.
// Returns (rawOFI, normalizedOFI) where normalizedOFI ∈ [-1, 1].
// The first call seeds state and returns (0, 0).
func (s *ofiState) update(ob OrderBook) (raw, norm float64) {
	if len(ob.Bids) == 0 || len(ob.Asks) == 0 {
		return 0, 0
	}
	bid, _ := strconv.ParseFloat(ob.Bids[0].Price, 64)
	ask, _ := strconv.ParseFloat(ob.Asks[0].Price, 64)
	bidQ, _ := strconv.ParseFloat(ob.Bids[0].Size, 64)
	askQ, _ := strconv.ParseFloat(ob.Asks[0].Size, 64)

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.count == 0 {
		s.prevBid, s.prevAsk = bid, ask
		s.prevBidQ, s.prevAskQ = bidQ, askQ
		s.count = 1
		return 0, 0
	}

	const eps = 1e-9

	// Bid contribution
	if bid > s.prevBid+eps {
		raw += bidQ
	} else if math.Abs(bid-s.prevBid) < eps {
		raw += bidQ - s.prevBidQ
	} else {
		raw -= s.prevBidQ
	}

	// Ask contribution
	if ask < s.prevAsk-eps {
		raw -= askQ
	} else if math.Abs(ask-s.prevAsk) < eps {
		raw += s.prevAskQ - askQ
	} else {
		raw += s.prevAskQ
	}

	// Store in ring buffer
	s.ring[s.head%ofiWindow] = raw
	s.head++
	if s.count < ofiWindow+1 {
		s.count++
	}

	s.prevBid, s.prevAsk = bid, ask
	s.prevBidQ, s.prevAskQ = bidQ, askQ

	// Normalize raw against max abs value in window
	n := ofiWindow
	if filled := s.count - 1; filled < n {
		n = filled
	}
	maxAbs := 0.0
	for i := 0; i < n; i++ {
		if a := math.Abs(s.ring[i]); a > maxAbs {
			maxAbs = a
		}
	}
	if maxAbs > 0 {
		norm = math.Max(-1, math.Min(1, raw/maxAbs))
	}
	return raw, norm
}
