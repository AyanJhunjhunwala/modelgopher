volume based strats for prediction markets.

Prediction markets — especially sports — swing hard and often revert. The thesis: most participants are acting on stale or delayed information, so large order-flow imbalances at the top of book are a leading signal, not a lagging one. This TUI exposes that signal in real time.

---

## CLI Usage

**Requirements:** Go 1.22+

```bash
cd modelgopher
go run .
```

- Type a market name and press `enter` to search across all ~7500 active Polymarket markets
- Press `tab` on the search screen to instantly load the top 20 markets by 24h volume
- Use `↑/↓` to navigate results, `enter` to select, `esc` to go back
- Selected market shows Yes/No price bars + live bid/ask depth from the CLOB, refreshed every second
- No API key required for browsing — uses Polymarket's public Gamma and CLOB APIs

---

## OFI — Order Flow Imbalance

OFI is a real-time signal derived from changes at the **best bid and ask** between consecutive order book snapshots. It measures net buying vs selling pressure at the touch, before any trade is reported.

### Formula (Cont, Kukanov & Stoikov 2014)

For each book update, compare the new best bid/ask to the previous:

**Bid contribution** (buy pressure):
| Condition | Contribution |
|-----------|-------------|
| bid price rose | `+new_bid_qty` |
| bid price unchanged | `+(new_bid_qty − prev_bid_qty)` |
| bid price fell | `−prev_bid_qty` |

**Ask contribution** (sell pressure, subtracted):
| Condition | Contribution |
|-----------|-------------|
| ask price fell | `−new_ask_qty` |
| ask price unchanged | `+(prev_ask_qty − new_ask_qty)` |
| ask price rose | `+prev_ask_qty` |

```
OFI(t) = bid_contribution + ask_contribution
```

Positive OFI → net buy pressure. Negative → net sell pressure.

### Display

The OFI row appears between the spread line and the bid ladder:

```
├── OFI    +312.0  ████████████░░  buy
├── OFI     -89.0  ████░░░░░░░░░░  sell
├── OFI      +0.0  ░░░░░░░░░░░░░░  neutral
```

- **Raw value**: cumulative contracts shifted at the touch since last tick
- **Bar**: normalized to `[-1, 1]` against the max absolute OFI seen in the last 30 ticks — shows relative strength, not absolute size
- **Label**: `buy` (>0.1), `sell` (<−0.1), or `neutral`

### Interpretation for sports markets

Sports markets have layered participant latency — some people trade on live data, others on degraded feeds or news that's 2–3 minutes late. A sustained positive OFI before a price move often means informed buyers are absorbing the ask. A sharp OFI reversal can signal an overextension worth fading.

---

## Logging

Press **`l`** while viewing any market to start writing OFI snapshots to a CSV file. Press **`l`** again to stop. A `[LOG]` badge appears in the top-right corner when active.

**Output file:** `ofi_<slug>_<YYYYMMDD_HHMMSS>.csv` in the working directory.

**Columns:**

| Column | Description |
|--------|-------------|
| `timestamp_ms` | Unix milliseconds |
| `slug` | Market slug |
| `ofi_raw` | Raw OFI for this tick |
| `ofi_norm` | Normalized OFI ∈ `[-1, 1]` |
| `best_bid` | Best bid price |
| `best_ask` | Best ask price |
| `spread` | `best_ask − best_bid` |

**Example analysis in Python:**
```python
import pandas as pd
df = pd.read_csv("ofi_some-market_20260328_141500.csv")
df["timestamp"] = pd.to_datetime(df["timestamp_ms"], unit="ms")
df.set_index("timestamp", inplace=True)
df["ofi_raw"].rolling(10).mean().plot(title="Rolling OFI")
```

---

## TODO

- **Trade recorder** — subscribe to the Polymarket WebSocket trade feed and log every fill as `{timestamp, side, size, price, token_id}` into a ring buffer of the last 1000 trades

- **Chunk OFI** — split the ring buffer into rolling chunks of 50 trades each; compute `OFI = (buy_vol − sell_vol) / (buy_vol + sell_vol)` per chunk so the signal is trade-driven rather than quote-driven

- **Rolling z-score** — maintain a running mean and std of OFI across the last 20 chunks; flag the current chunk as anomalous when `|z| > 2.5`

- **Alert with context** — when flagged, snapshot `{timestamp, price, OFI, z-score}` to a log file for post-hoc comparison against news timelines

- **Backtest after resolution** — once a market resolves, check whether pre-event anomalies pointed toward the correct outcome at above-chance rates (binomial test against 50% baseline)
