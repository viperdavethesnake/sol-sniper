# Product Requirements Document — Sol Sniper SS-V1

## Overview

SS-V1 is a Solana MEV sniper bot that detects new token launches on Raydium AMM V4 and Pump.fun, evaluates them against safety filters, and executes buys via Jito bundles when a signal passes all gates. It is a chain-specific port of the existing base-snipper (Base chain), sharing the same ML training data format, Redis schema, and Prometheus metric names to enable cross-chain model training.

## Goals

1. Detect new Raydium pools and Pump.fun launches within 1–2 slots of creation
2. Filter rugs and honeypots before capital is committed
3. Execute buys exclusively via Jito bundles — standard txs are front-run on Solana in 2026
4. Produce JSONL training records (`/data/sol_sniper_training_v1.jsonl`) structurally identical to base-snipper records, with three additional Solana-specific fields
5. Expose the same Prometheus metrics and REST API as base-snipper so Grafana dashboards and the ML pipeline require no changes

## Non-Goals

- Limit order / DCA strategies
- Multi-hop routing (all swaps are single-pool)
- Geyser/gRPC account streaming (standard `logsSubscribe` WebSocket is sufficient for V1)
- Cross-chain arbitrage

## User Stories

**Operator**
- I can set `SIMULATION_MODE=true` to observe token flow and filter decisions without spending SOL
- I can POST to `/api/killswitch` to halt buying and dump all open positions immediately
- I can view real-time P&L, active positions, and filter breakdown in Grafana at `192.168.33.212:3000`

**ML Pipeline**
- Every token seen (accepted or rejected) produces a JSONL record with consistent schema across Base and Solana chains
- `is_pump_fun`, `priority_tip_sol`, and `bundle_landed` fields enable Solana-specific model features

## Functional Requirements

### Listener
- Subscribe to Raydium AMM V4 program logs via `logsSubscribe`; detect `initialize2` instruction
- Subscribe to Pump.fun program logs; detect `create` instruction
- Extract mint address, pool/bonding-curve address, and initial SOL liquidity from vault balances
- Reconnect with 5-second backoff on WebSocket error

### Analyzer (filters applied in order)
1. **Dedup** — skip if token seen in last 48 hours (Redis)
2. **Freeze authority** — filter if `freezeAuthority != null`
3. **Mint authority** — filter if `mintAuthority != null`
4. **Liquidity** — filter if initial SOL < `MIN_LIQUIDITY_SOL`
5. **Holder concentration** — filter if top 2 holders own > 80% of supply
6. **Friction simulation** — constant-product round-trip sim; filter if loss > `MAX_FRICTION_PCT`
7. **Entry impact** — record `entry_price_impact_pct` for ML

### Executor
- Submit buy as Jito bundle via HTTP JSON-RPC (`POST https://{JITO_URL}/api/v1/bundles`)
- Scale Jito tip 1×–5× base based on pool liquidity (heat signal)
- Poll `GetSignatureStatuses` for confirmation (60-second timeout)
- Track position in Redis; monitor every `POSITION_MONITOR_INTERVAL` seconds for TP/SL/timeout
- On stop-loss: blacklist token deployer

### API
- All endpoints from base-snipper preserved unchanged
- `/health` includes `simulation_mode` and `rpc_endpoint` in response

## Filter Reasons (for Prometheus label + JSONL)

`freeze_authority_set`, `mint_authority_set`, `rug_concentration`, `insufficient_liquidity`, `no_sol_pair`, `honeypot`, `high_friction`

## Success Metrics

| Metric | Target |
|---|---|
| Detection latency | < 2 slots after pool creation |
| Filter false-positive rate | < 5% (validated against historical data) |
| Bundle landing rate at base tip | > 40% |
| Bundle landing rate at 5× tip | > 90% |
| JSONL record completeness | 100% of seen tokens |
