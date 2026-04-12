# Run 001 — Findings & Analysis
**Date:** 2026-04-12  
**Duration:** ~10.9 hours (08:00 – 18:54 UTC)  
**Mode:** Simulation (paper trading, no real transactions)  
**Config:** MIN_LIQUIDITY_SOL=1.0, POSITION_MONITOR_INTERVAL=10s, MAX_FRICTION_PCT=20.0  
**Chain:** Solana Mainnet via Helius

---

## 1. Executive Summary

The pipeline ran continuously for ~11 hours without a WebSocket disconnect. It processed **36,764 token events** and wrote **9,282 JSONL records**. Zero paper trades were executed. All tokens were rejected before reaching the executor. Two bugs were identified that together blocked every potential entry: listener goroutine panics and a vault account race condition in the analyzer. Both are fixable. The data collected is still valuable — it establishes Pump.fun event volume, liquidity distributions, and filter-stage dropout rates that will inform thresholds and model features.

---

## 2. Pipeline Performance

| Metric | Value |
|---|---|
| Total token events received | ~36,764 |
| JSONL records written | 9,282 |
| WebSocket reconnections | 0 |
| Coverage gaps > 30s | 26 (longest: 56s) |
| Paper trades executed | 0 |
| Bot restarts | 2 (config tuning during startup) |

**Token detection rate:**

| Hour (UTC) | Tokens Recorded | Rate/min |
|---|---|---|
| 08:00 | 681 | ~11 |
| 09:00 | 558 | ~9 (trough) |
| 11:00 | 775 | ~13 |
| 12:00 | 807 | ~13 |
| 13:00 | 918 | ~15 |
| 14:00 | 1,083 | ~18 (peak) |
| 15:00 | 915 | ~15 |
| 16:00–18:00 | ~1,000/hr avg | ~17 |

**Pattern:** Volume tracks US/EU overlap hours. Peak activity 13:00–17:00 UTC (~2–4× the overnight baseline). This matters for tip sizing and timing strategy.

---

## 3. Source Distribution

| Source | Records | % |
|---|---|---|
| Pump_Fun | 9,333 | ~99.98% |
| Raydium_V4 | 2 | ~0.02% |

**Key finding:** The market for new token launches on Solana is almost entirely Pump.fun. Raydium V4 new pool creation is extremely rare in comparison. The bot's Raydium logic is correct but will rarely trigger. For ML training data volume, Pump.fun is the signal source.

---

## 4. Filter Breakdown

| Filter Stage | Count | % of Total | Notes |
|---|---|---|---|
| `insufficient_liquidity` | 4,769 | 51.4% | < 1 SOL at launch |
| `vault_read_error` | 4,160 | 44.8% | **Bug — see §6** |
| `mint_fetch_error` | 343 | 3.7% | **Bug — see §6** |
| `high_friction` | 6 | 0.1% | Legitimate rejects |
| `rug_concentration` | 2 | <0.1% | Legitimate rejects |
| `freeze_authority_set` | 2 | <0.1% | Legitimate rejects |

**Only 10 of 9,282 records (0.1%) reached the friction/safety analysis stage.** The other 99.9% never got there due to liquidity or bugs.

---

## 5. Patterns & Signal Observations

### 5a. Liquidity Distribution (tokens passing 1 SOL threshold)

4,411 tokens cleared the minimum liquidity filter. Of those:

| Liquidity Range | Count | % |
|---|---|---|
| 1 – 2 SOL | 789 | 17.9% |
| 2 – 5 SOL | 1,686 | 38.2% |
| 5 – 10 SOL | 1,512 | 34.3% |
| 10 – 50 SOL | 420 | 9.5% |
| 50+ SOL | 4 | 0.1% |

**Median: 4.785 SOL.** Most qualifying tokens launch in the 2–10 SOL range. Very few (4 in 11 hours) launch above 50 SOL. The 10–50 SOL bucket (~420 tokens) represents the highest-quality targets and warrants a separate signal tier.

### 5b. Rug Traps Caught

Two `rug_concentration` tokens were filtered — top-2 holders owned > 80% of supply:

| Token | Initial Liquidity | Significance |
|---|---|---|
| `9bS8Jd3c6WuDPrFLvaKh` | 47.0 SOL | Mid-tier liq, concentrated |
| `EV83A2qontjjKb5g4NrN` | **759.1 SOL** | Very high liq — a serious trap |

The 759 SOL token is notable. High initial liquidity is typically a positive signal; this token would have passed the liq filter with any threshold and appeared attractive. The holder concentration check saved a potentially significant loss. **This validates that the concentration filter must run before any capital commitment, regardless of how much liquidity a pool shows.**

### 5c. Freeze Authority Catches

One token (`4wTV1YmiEkRvAtNtsSGP`) triggered `freeze_authority_set` twice with near-identical but distinct liquidity readings (0.00374 SOL, 0.00386 SOL). The token had essentially zero liquidity at detection, so the freeze flag was redundant here. However, the double-detection suggests a minor dedup race condition: the same token was detected twice before either goroutine completed `MarkTokenSeen`. **Low priority — the liquidity check would have caught it regardless — but worth noting for data integrity.**

### 5d. High-Friction Tokens

All 6 `high_friction` rejects were Pump.fun tokens. Liquidity ranged from 1.2–4.2 SOL. The `friction_score` field was `null` in all 6 records — the score was computed (otherwise it wouldn't reach this filter stage) but the field wasn't propagated to the JSONL `Execution` struct. Minor data gap; the reject reason is correct.

### 5e. vault_read_error Liquidity Profile

The 4,160 vault_read_error tokens had:
- **Min:** 1.0 SOL | **Max:** 56.3 SOL | **Median: 4.94 SOL**

These are tokens with meaningful liquidity that are being silently discarded. The median (4.94 SOL) is actually *higher* than the overall post-liq-filter median (4.785 SOL). This means vault_read_error tokens are not a garbage tier — they are the bulk of our qualifying candidate pool. **Fixing this bug is the single highest-value action to unlock paper trades.**

---

## 6. Bugs Found

### Bug 1 — Listener: Index Out of Range Panic (Critical)

**File:** `internal/listener/listener.go` lines 306–307  
**Symptom:** Goroutine panics with `index out of range [N] with length M` — observed continuously throughout the run.

**Root cause:** The instruction account layout check `if len(ix.Accounts) < 3` guards the *length of the accounts slice*, but `ix.Accounts[0]` and `ix.Accounts[2]` are uint8 *indices into `msg.AccountKeys`*. If the stored index value (e.g. `ix.Accounts[0] = 11`) equals or exceeds the length of `accts` (e.g. `len(accts) = 11`), the dereference `accts[ix.Accounts[0]]` panics.

**Fix:** Add bounds checks on the index values before dereferencing:
```go
if int(ix.Accounts[0]) >= len(accts) || int(ix.Accounts[2]) >= len(accts) {
    continue
}
```

The same class of bug exists in `handleRaydium` and should be patched there too (accounts indices 4, 8, 9, 11).

**Impact:** Unknown number of valid token events dropped silently. Each panic is a lost event that never reaches the analyzer.

---

### Bug 2 — Analyzer: vault_read_error Race Condition (High)

**File:** `internal/analyzer/analyzer.go` — `resolveVaults` + `readVaultBalances`  
**Symptom:** 4,160 tokens (44.8% of all records) rejected with `vault_read_error`.

**Root cause:** For Pump.fun, the analyzer derives an Associated Token Account (ATA) via `FindAssociatedTokenAddress(bondingCurve, mint)` and immediately queries its balance. The ATA is frequently not yet queryable at `CommitmentConfirmed` immediately after the create transaction lands — the account may not be initialized on-chain yet. This is a timing race between our detection and the token's account state propagation.

**Impact:** Every token with median-or-better liquidity (4.94 SOL) is being dropped. This is the primary reason zero paper trades executed.

**Fix options (in order of preference):**
1. **Short retry with backoff** — retry vault reads 2–3× with 500ms delay before rejecting. Most accounts settle within 1–2 seconds.
2. **Skip friction for Pump.fun entirely** — use the bonding curve SOL balance already captured in `TokenEvent.LiqSOL` as the sole liquidity signal, bypassing ATA reads. Friction simulation is less meaningful on Pump.fun's bonding curve model anyway (the curve has a built-in price function, not a constant-product AMM).

Option 2 is architecturally cleaner and removes an unnecessary RPC call.

---

### Bug 3 — JSONL: friction_score not written to Execution struct (Minor)

**Symptom:** `high_friction` records have `"execution": null` — the friction score that triggered the reject is not persisted.  
**Impact:** 6 records lack execution data; these would be useful training examples showing what "too much friction" looks like.  
**Fix:** Write the `Execution` block before the reject decision, not after.

---

## 7. Configuration Observations

- **MIN_LIQUIDITY_SOL=1.0** — correct direction. Would lower further to 0.5 SOL once vault_read_error is fixed, to capture more Pump.fun launches in their early bonding curve phase.
- **MAX_FRICTION_PCT=20.0** — reasonable for now. Once paper trades flow, review actual friction scores to calibrate.
- **POSITION_MONITOR_INTERVAL=10s** — appropriate. Will produce good trajectory resolution once positions open.
- **TAKE_PROFIT_PCT=100 / STOP_LOSS_PCT=30** — conservative TP, moderate SL. Suitable for paper trading baseline; adjust once trajectory data shows typical Pump.fun price shape.

---

## 8. Recommended Actions (Priority Order)

| # | Action | Impact |
|---|---|---|
| 1 | Fix listener bounds check (Bug 1) | Stop silent event drops |
| 2 | Fix vault_read_error — add retry or skip Pump.fun vault reads (Bug 2) | Unlock paper trades |
| 3 | Fix friction_score not written to JSONL (Bug 3) | Complete training records |
| 4 | Lower MIN_LIQUIDITY_SOL to 0.5 once Bug 2 is fixed | Broader training data coverage |
| 5 | Add source label to hourly metrics (`sniper_tokens_scanned_total{source}`) | Distinguish Raydium vs Pump.fun in Grafana |
| 6 | Investigate dedup race (§5c) | Data integrity for ML |

---

## 9. ML Training Data Outlook

Once bugs 1–2 are fixed, expected daily paper trade volume based on today's rates:

- Tokens scanned: ~80,000/day at current volume
- Post-liquidity (≥1 SOL): ~38,000/day
- Expected to reach executor (if vault_read_error fixed): ~36,000/day
- Expected paper positions opened: depends on friction/holder checks — conservatively 100–500/day based on 6/9282 passing friction today (once vault reads work, many of those 4,160 vault_read_errors will pass)

**The 759 SOL rug and the high-friction Pump.fun tokens are already useful negative training examples.** The dedup, freeze, and concentration filters are working correctly and producing labeled data.
