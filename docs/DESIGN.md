# Technical Design — Sol Sniper SS-V1

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│  Solana Mainnet                                          │
│  logsSubscribe (WSS)                                    │
│    ├─ Raydium AMM V4  (initialize2)                     │
│    └─ Pump.fun        (create)                          │
└────────────────┬────────────────────────────────────────┘
                 │ *TokenEvent
                 ▼
         ┌───────────────┐   eventCh(256)   ┌─────────────────┐
         │   listener    │ ───────────────► │    analyzer     │
         │ listener.go   │                  │  analyzer.go    │
         └───────────────┘                  └────────┬────────┘
                                                     │ *TradeSignal
                                            tradeCh(64)
                                                     │
                                                     ▼
                                            ┌─────────────────┐
                                            │    executor     │
                                            │  executor.go    │
                                            │                 │
                                            │  Jito HTTP RPC  │
                                            │  MonitorPos     │
                                            └────────┬────────┘
                                                     │
                         ┌───────────────────────────┼───────────────────┐
                         ▼                           ▼                   ▼
                      Redis                    Prometheus            JSONL file
                  (dedup, positions,         (metrics scrape      (/data/sol_sniper
                   leaderboard, P&L)          by Grafana)          _training_v1.jsonl)
```

## Network Layout

macvlan IP `192.168.33.212`. Redis owns the interface; all services (`bot`, `prometheus`, `grafana`) share Redis's network namespace via `network_mode: service:redis`. This means `localhost` resolves the same way for all services — Prometheus scrapes `localhost:8080`, Grafana talks to `localhost:9090`.

| Container | IP | Ports |
|---|---|---|
| base-snipper-redis | 192.168.33.211 | (shared with base-snipper stack) |
| sol-sniper-redis | 192.168.33.212 | 6379 |
| sol-sniper-bot | (shared) | 8080 |
| sol-sniper-prometheus | (shared) | 9090 |
| sol-sniper-grafana | (shared) | 3000 |

## Key Design Decisions

### Jito via HTTP, not gRPC
The jito-go SDK requires generated proto stubs and a gRPC connection. For V1, the HTTP JSON-RPC endpoint (`POST /api/v1/bundles`) is simpler, has no extra dependencies, and is the approach documented by Jito for external integrators. Bundle payload is base64-encoded serialized transactions.

### Instruction account parsing
`msg.AccountKeys` is a deduplicated flat list across ALL instructions in the transaction. Account positions in this list cannot be assumed to match per-instruction layout. The correct approach: iterate `msg.Instructions`, match `accts[ix.ProgramIDIndex]` against the target program, then use `ix.Accounts[N]` as indices into `msg.AccountKeys`.

### Constant-product quoting (Shadow Eyes)
Raydium V4 uses x·y=k with 0.25% fee. Buy simulation:
```
amountOut = (amountIn × 9975 × reserveOut) / (reserveIn × 10000 + amountIn × 9975)
```
Round-trip (buy then sell) gives friction %. This is done offline from vault account data — no `eth_call` equivalent needed. Vault pubkeys are at byte offsets 336 and 368 in the Raydium pool state account.

### Dynamic Jito tipping
```go
multiplier := math.Max(1.0, math.Min(5.0, liqSOL/10.0))
tipLamports = uint64(baseTipSOL * multiplier * 1e9)
```
A 50 SOL pool gets 5× tip; a 5 SOL pool gets 1×. The `priority_tip_sol` and `bundle_landed` fields in the JSONL output let the ML model learn the optimal tip curve.

### Redis schema (differences from base-snipper)
- `pnl:sol` — P&L float accumulator (base-snipper uses `pnl:eth`)
- All other keys identical: `seen:token:{addr}`, `positions:open`, `pnl:sol`, `leaderboard:gainers`, `leaderboard:losers`, `trajectory:{addr}`, `killswitch`

## Package Map

| Package | Responsibility |
|---|---|
| `config` | Load env → `Config` struct; `MaxBuyLamports()` helper |
| `metrics` | All Prometheus metric definitions (counters, histograms, gauges) |
| `cache` | Redis client; domain methods: `IsSeenToken`, `SavePosition`, `IncrPnL`, `Leaderboard`, `Trajectory`, `KillSwitch` |
| `dataset` | Append-only JSONL writer; `Record` struct with `Metadata`, `Execution`, `Trajectory`, `FinalLabel` |
| `listener` | WS `logsSubscribe` → parse tx → emit `*TokenEvent` |
| `analyzer` | Safety filter pipeline → emit `*TradeSignal`; one goroutine per event |
| `executor` | Jito bundle submit, confirmation polling, position monitor, TP/SL/kill-switch |
| `api` | Gin HTTP server; all REST endpoints + `/metrics` scrape |

## Data Flow: Token Accepted

```
TokenEvent{Mint, Pool, Source, LiqSOL}
  → dedup check (Redis 48h TTL)
  → freeze/mint authority check (GetAccountInfo)
  → liquidity check
  → holder concentration check (GetTokenLargestAccounts)
  → vault resolution (pool state account read)
  → friction simulation (CP formula)
  → TradeSignal emitted
  → executor: kill-switch check → position dedup
  → build swap + tip instructions
  → GetLatestBlockhash → sign → base64 encode
  → POST /api/v1/bundles (Jito HTTP)
  → poll GetSignatureStatuses (60s timeout)
  → on confirm: save Position to Redis, write JSONL Execution record
  → MonitorPositions: poll price → TP/SL/timeout → sell bundle → close JSONL record
```

## Solana vs Base-Snipper Mapping

| Base-Snipper | Sol-Sniper |
|---|---|
| `eth_getLogs` WebSocket | `logsSubscribe` on program IDs |
| `PairCreated` event | `initialize2` (Raydium) / `create` (Pump.fun) |
| `liquidityETH` from reserves | `liquiditySOL` from vault `GetTokenAccountBalance` |
| ERC-20 honeypot simulation | `freezeAuthority` + `mintAuthority` checks on SPL mint |
| Flashbots bundle | Jito bundle (HTTP JSON-RPC) |
| `eth_call` quoting | Offline CP formula from vault account data |
| Gas priority fee | Jito tip (scaled by pool heat) |
| `pnl_eth` metric | `pnl_sol` metric |
