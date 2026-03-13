volume based strats for prediction markets.

I think right now, I've noticed that reversion trading makes sense for prediction markets because of how much they actually swing. This is specicially for sport games. So I'm going to test out a volume indicator and see if I can do reversion trading by actually beating out most of the sellers or buyers purely from a volume direction perspective.

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
- No API key required — uses Polymarket's public Gamma and CLOB APIs