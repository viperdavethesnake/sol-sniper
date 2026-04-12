# Sol Sniper — SS-V1

Solana MEV sniper bot. Port of `../base-snipper` (Base chain) to Solana. Maintains full data-format and observability parity so the shared ML pipeline can consume training data from both chains in a unified format.

**Access after `docker compose up`:**
- Bot API / Health: `http://192.168.33.212:8080`
- Prometheus: `http://192.168.33.212:9090`
- Grafana: `http://192.168.33.212:3000` (admin / admin)

## Quick Start

```bash
cp .env.example .env
# fill in RPC_URL, RPC_WS_ENDPOINT, JITO_BLOCK_ENGINE_URL, BOT_PRIVATE_KEY
docker compose up --build -d
docker compose logs -f bot
```

## Build (without Docker)

```bash
go build -ldflags "-s -w" -o bot ./cmd/bot
SIMULATION_MODE=true ./bot
```

## Environment Variables

See `.env.example` for all variables. Critical ones:

| Variable | Description |
|---|---|
| `RPC_URL` | Helius/Triton HTTPS endpoint |
| `RPC_WS_ENDPOINT` | Helius/Triton WSS endpoint |
| `JITO_BLOCK_ENGINE_URL` | e.g. `frankfurt.mainnet.block-engine.jito.wtf` |
| `BOT_PRIVATE_KEY` | Base58 keypair |
| `SIMULATION_MODE` | `true` to log bundles without submitting |
| `MIN_LIQUIDITY_SOL` | Minimum pool SOL to consider (default 5.0) |
| `MAX_BUY_SOL` | Max SOL per snipe (default 0.1) |
| `MAX_FRICTION_PCT` | Filter threshold for round-trip loss % (default 20) |

## Pipeline

```
logsSubscribe (WS)
  └─ Raydium initialize2 / Pump.fun create
       └─ listener → eventCh(256) → analyzer → tradeCh(64) → executor
                                                                └─ Jito bundle (HTTP)
                                                                └─ MonitorPositions (TP/SL)
```

## API Endpoints

| Method | Path | Description |
|---|---|---|
| GET | `/health` | Liveness + redis status |
| GET | `/api/stats` | Counters, P&L, kill-switch state |
| GET | `/api/positions` | Open positions |
| GET | `/api/leaderboard` | Top 5 gainers / losers |
| GET | `/api/trajectory/:token` | Price snapshots for a token |
| POST | `/api/killswitch` | `{"active": true}` arms, `false` disarms |
| GET | `/metrics` | Prometheus scrape endpoint |

## Network

macvlan IP `192.168.33.212` on `macvlan_net`. Redis owns the interface; bot, Prometheus, and Grafana share its network namespace. Base-snipper is `.211`.
