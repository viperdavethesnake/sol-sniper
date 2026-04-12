# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**SS-V1 (Solana Sniper V1)** — a Go MEV sniper bot for Solana. It is the Solana port of `../base-sniper` and must maintain data-format and observability parity with it so the shared ML pipeline can consume data from both chains in a unified format.

The `starthere` file is the canonical specification. Reference `../base-sniper` as the template for all architectural patterns.

## Build & Run

```bash
# Build
go build -ldflags "-s -w" -o bot ./cmd/bot

# Run (requires .env)
./bot

# Docker
docker compose up --build

# Tests
go test ./...
go test ./internal/analyzer/...   # single package
```

## Stack

- **Language:** Go 1.22 (match base-sniper)
- **RPC:** Jito Block Engine + Helius/Triton private RPC
- **Cache:** Redis — deduplication, trajectories, positions, leaderboard
- **Metrics:** Prometheus scraped by Grafana
- **Key libraries:** `gagliardetto/solana-go`, `redis/go-redis v9`, `prometheus/client_golang`, `rs/zerolog`
- **Jito integration:** HTTP JSON-RPC (`POST https://{JITO_URL}/api/v1/bundles`) — no gRPC, no jito-go SDK

## Module Structure

Mirror base-sniper's layout exactly:

```
cmd/bot/main.go             # bootstrap: config → redis → channels → components → goroutines
internal/
  config/config.go          # Config struct + Load() from env
  listener/listener.go      # logsSubscribe WebSocket, emits *TokenEvent
  analyzer/analyzer.go      # safety checks, emits *TradeSignal
  executor/executor.go      # Jito bundle submission, position tracking
  metrics/metrics.go        # all Prometheus metric definitions
  cache/cache.go            # Redis client + domain helpers
  dataset/writer.go         # JSONL training data archiver
  api/server.go             # REST API + /metrics scrape endpoint
```

## Pipeline Architecture

The pipeline is identical to base-sniper — three goroutines connected by buffered channels:

```
listener.Run(ctx)  →  eventCh (256)  →  analyzer.Run(ctx, eventCh)  →  tradeCh (64)  →  executor.Run(ctx, tradeCh)
                                                                                            executor.MonitorPositions(ctx)
```

Analyzer fans out one goroutine per event. Executor spawns one goroutine per trade signal.

## Key Structs to Define

**TokenEvent** (listener → analyzer):
```go
type TokenEvent struct {
    Mint       solana.PublicKey   // new token mint address
    Pool       solana.PublicKey   // Raydium pool / Pump.fun bonding curve
    Source     string             // "raydium" | "pump_fun"
    LiqSOL     float64            // initial vault SOL balance
    Slot       uint64
    TxSig      string
    DetectedAt time.Time
}
```

**TradeSignal** (analyzer → executor):
```go
type TradeSignal struct {
    Mint              solana.PublicKey
    Pool              solana.PublicKey
    Source            string        // "raydium" | "pump_fun"
    LiqSOL            float64
    FrictionScore     float64       // simulated round-trip loss %
    TokensOut         uint64        // expected tokens for MaxBuyLamports
    EntryPriceImpactPct float64
    IsPumpFun         bool
    Symbol            string
    DetectedAt        time.Time
}
```

## Solana-Specific Logic vs Base-Sniper Equivalents

| Base-Sniper | Sol-Sniper |
|---|---|
| `eth_getLogs` / WebSocket `logs` filter | `logsSubscribe` on Raydium Authority V4 or Pump.fun program IDs |
| PairCreated / PoolCreated events | `initialize2` instruction (Raydium) or `create` instruction (Pump.fun) |
| `liquidityETH` from pair reserves | `liquiditySOL` from Vault Account balances |
| `freezeAuthority` n/a (ERC-20) | **Must** check `freezeAuthority == null`; non-null = instant filter |
| `mintAuthority` via `owner()` | Check `mintAuthority` renounced on SPL mint account |
| Top-holder rug check (skip EVM) | Query top 10 holders; filter if bonding curve >80% held by 2 wallets |
| Flashbots bundles | **Jito Bundles** — mandatory; standard txs get front-run |
| Gas priority fee | **Jito tip** — scale with token heat; track `priority_tip_sol` for ML |
| `eth_call` for quoting (slow) | Local constant-product calc from `OpenOrders` + `BaseVault`/`QuoteVault` |
| GasPriceOracle L1 fee check | n/a; replace with Jito tip abort if tip > X% of buy amount |

## Prometheus Metrics

Defined in `internal/metrics/metrics.go`. Copy all Vec definitions from `../base-sniper/internal/metrics/metrics.go` with these renames:

| base-sniper metric | sol-sniper metric |
|---|---|
| `sniper_pnl_eth` | `sniper_pnl_sol` |
| `sniper_gas_priority_fee_gwei` | `sniper_jito_tip_sol` |
| `sniper_rpc_latency_seconds{method}` | same |

All other metric names are identical. Add one new gauge:

- `sniper_jito_tip_success_ratio` — rolling ratio of tip amounts to landing success (for ML)

Filter reasons for `sniper_tokens_filtered_total{reason}` counter:
`no_sol_pair`, `freeze_authority_set`, `mint_authority_set`, `rug_concentration`, `insufficient_liquidity`, `honeypot`, `high_friction`

## JSONL Training Data

File: `/data/sol_sniper_training_v1.jsonl`

Record structure mirrors base-sniper's `dataset.Record` exactly. Add these fields to `Metadata`:

```go
type Metadata struct {
    // ... same as base-sniper ...
    IsPumpFun       bool    `json:"is_pump_fun"`
    PriorityTipSOL  float64 `json:"priority_tip_sol"`   // Jito tip paid (lamports / 1e9)
    BundleLanded    bool    `json:"bundle_landed"`       // for tip-vs-success ML training
}
```

`FinalLabel` values are identical to base-sniper: `REJECTED_*`, `TAKE_PROFIT`, `STOP_LOSS`, `KILL_SWITCH`, `TIMEOUT`.

## Redis Keys

Most keys are identical to base-sniper. One difference: `pnl:sol` (not `pnl:eth`) is the P&L accumulator key. `LeaderboardEntry` uses `DeltaSOL` and `TrajectorySnap` uses `CurrentSOL`. All other key patterns (`seen:token:`, `positions:open`, leaderboard sorted sets, trajectory lists, kill switch) are unchanged.

## Config / Environment Variables

Mirror base-sniper's `Config` struct. Swap ETH-specific fields:

```
JITO_BLOCK_ENGINE_URL=frankfurt.mainnet.block-engine.jito.wtf
RPC_URL=https://mainnet.helius-rpc.com/?api-key=${HELIUS_KEY}
BOT_PRIVATE_KEY=<base58 keypair>
MIN_LIQUIDITY_SOL=5.0         # replaces MIN_LIQUIDITY_ETH
MAX_BUY_SOL=0.1               # replaces MAX_BUY_ETH
MAX_FRICTION_PCT=20.0
TAKE_PROFIT_PCT=100
STOP_LOSS_PCT=30
SLIPPAGE_PCT=15
REDIS_ADDR=redis:6379
SIMULATION_MODE=false
```

## Docker

Shares `redis`, `prometheus`, and `grafana` services from the parent lab stack via `internal_stack` network. The sol-sniper container is standalone; it does not fork or replace the base-sniper container.

```yaml
sol-sniper-bot:
  build: ./sol-sniper
  volumes:
    - ./data:/data
  environment:
    - JITO_BLOCK_ENGINE_URL=...
    - RPC_URL=...
  networks:
    - internal_stack
  depends_on:
    - redis
    - prometheus
```

## Dockerfile

Use the same two-stage pattern as `../base-sniper/bot/Dockerfile`:
- Builder: `golang:1.22-alpine`, `CGO_ENABLED=0`, strip binary with `-s -w`
- Final: `scratch` with CA certs + tzdata copied in, expose port 8080
