// Package analyzer performs Solana-specific token safety checks and emits
// TradeSignals for tokens that pass all filters.
//
// Safety pipeline (in order):
//  1. Deduplication       — Redis 48h, atomic SET NX
//  2. Freeze authority    — freezeAuthority must be null
//  3. Mint authority      — mintAuthority must be renounced
//  4. Liquidity threshold — ≥ MinLiqSOL in pool
//  5. Graduation guard    — Pump.fun tokens ≥ 70 SOL are near migration; skip
//  6. Holder concentration — top-2 holders must not control > 80% supply
//  7. Token metadata      — fetch symbol via Metaplex PDA
//  8. Friction simulation — offline round-trip estimate (protocol-aware)
//  9. Emit TradeSignal + PASSED_ANALYSIS JSONL record
package analyzer

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/rs/zerolog/log"

	"github.com/sol-sniper/bot/internal/cache"
	"github.com/sol-sniper/bot/internal/config"
	"github.com/sol-sniper/bot/internal/dataset"
	"github.com/sol-sniper/bot/internal/listener"
	"github.com/sol-sniper/bot/internal/metrics"
)

// pumpGraduationSOL is the approximate bonding-curve SOL balance at which
// Pump.fun migrates to Raydium. Tokens near this threshold face a brief
// un-sellable window and price gap during migration.
const pumpGraduationSOL = 85.0

// pumpGraduationGuardSOL is the liquidity level above which we skip the token.
// Set 15 SOL below graduation to avoid entering a position that may graduate
// before our monitor cycle has a chance to exit.
const pumpGraduationGuardSOL = 70.0

// Metaplex token metadata program ID.
var metaplexMetadataProg = solana.MustPublicKeyFromBase58("metaqbxxUerdq28cj1RbAWkYQm3ybzjb6a8bt518x1s")

// TradeSignal carries everything the executor needs to open a position.
type TradeSignal struct {
	Mint                solana.PublicKey
	Pool                solana.PublicKey // Raydium AMM ID or Pump.fun bonding curve
	Source              string           // "raydium" | "pump_fun"
	LiqSOL              float64
	FrictionScore       float64 // simulated round-trip loss %
	TokensOut           uint64  // expected raw token units for MaxBuyLamports
	EntryPriceImpactPct float64
	IsPumpFun           bool
	Symbol              string

	// Pool vault accounts — populated by analyzer, consumed by executor.
	BaseVault  solana.PublicKey // token vault
	QuoteVault solana.PublicKey // WSOL vault

	DetectedAt time.Time
}

// pumpFunHTTPClient is used for Pump.fun API symbol lookups — short timeout,
// non-critical (symbol fetch failure just leaves symbol blank).
var pumpFunHTTPClient = &http.Client{Timeout: 2 * time.Second}

// Analyzer reads TokenEvents, runs safety checks, and emits TradeSignals.
type Analyzer struct {
	cfg     *config.Config
	rpc     *rpc.Client
	cache   *cache.Client
	tradeCh chan<- *TradeSignal
	dw      *dataset.Writer
}

// New creates an Analyzer.
func New(cfg *config.Config, c *cache.Client, tradeCh chan<- *TradeSignal, dw *dataset.Writer) (*Analyzer, error) {
	return &Analyzer{
		cfg:     cfg,
		rpc:     rpc.New(cfg.RPCURL),
		cache:   c,
		tradeCh: tradeCh,
		dw:      dw,
	}, nil
}

// Run reads from eventCh and spawns one goroutine per event (fan-out pattern).
func (a *Analyzer) Run(ctx context.Context, eventCh <-chan *listener.TokenEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-eventCh:
			if !ok {
				return
			}
			go a.process(ctx, ev)
		}
	}
}

// process runs the full analysis pipeline for a single token event.
func (a *Analyzer) process(ctx context.Context, ev *listener.TokenEvent) {
	// Per-token deadline prevents goroutine leaks when RPC stalls.
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()

	start := time.Now()
	mintStr := ev.Mint.String()

	// Recover from unexpected panics (malformed on-chain data, nil pointer, etc.)
	// so a single bad token doesn't silently kill this goroutine.
	defer func() {
		if r := recover(); r != nil {
			metrics.AnalyzerPanics.Inc()
			log.Error().Interface("panic", r).Str("mint", mintStr).Msg("analyzer: panic recovered")
		}
	}()
	defer func() {
		metrics.AnalysisDuration.Observe(time.Since(start).Seconds())
	}()

	// ── 1. Deduplication (atomic SET NX — eliminates TOCTOU race) ────────────
	isNew, err := a.cache.TryMarkTokenSeen(ctx, mintStr)
	if err != nil {
		log.Warn().Err(err).Str("mint", mintStr).Msg("analyzer: dedup check failed")
	}
	if !isNew {
		return
	}
	_ = a.cache.Incr(ctx, "scanned")

	// Build metadata progressively — the reject closure captures it so every
	// JSONL record has all the features measured up to the point of rejection.
	meta := dataset.Metadata{
		DEX:                dexLabel(ev.Source),
		InitLiqSOL:         ev.LiqSOL,
		IsPumpFun:          ev.Source == "pump_fun",
		SlotAtDetection:    ev.Slot,
		DetectionLatencyMs: time.Since(ev.DetectedAt).Milliseconds(),
	}

	// execData is nil until after friction simulation; the reject closure captures
	// it so that high_friction records include the full Execution block.
	var execData *dataset.Execution

	reject := func(reason string) {
		metrics.TokensFiltered.WithLabelValues(reason).Inc()
		_ = a.cache.Incr(ctx, "filtered")
		log.Info().Str("mint", mintStr).Str("reason", reason).Msg("analyzer: token filtered")
		if a.dw != nil {
			a.dw.Write(dataset.Record{
				ScannedAt:    ev.DetectedAt,
				TokenAddress: mintStr,
				Metadata:     meta,
				Execution:    execData,
				Trajectory:   []dataset.TrajectoryPoint{},
				FinalLabel:   "REJECTED_" + reason,
			})
		}
	}

	// ── 2. Fetch mint account and check authorities ───────────────────────────
	mintInfo, err := a.getMintInfo(ctx, ev.Mint)
	if err != nil {
		log.Warn().Err(err).Str("mint", mintStr).Msg("analyzer: failed to fetch mint account")
		reject("mint_fetch_error")
		return
	}
	// Populate mint-derived fields even before the authority checks so that
	// reject records for freeze/mint-authority violations include them.
	meta.MintDecimals = mintInfo.Decimals
	meta.IsRenounced = mintInfo.MintAuthorityOption == 0

	// FreezeAuthorityOption == 1 means freeze authority is set (danger).
	if mintInfo.FreezeAuthorityOption != 0 {
		reject("freeze_authority_set")
		return
	}

	// MintAuthorityOption == 1 means new tokens can still be minted.
	if mintInfo.MintAuthorityOption != 0 {
		reject("mint_authority_set")
		return
	}

	// ── 3. Liquidity threshold ────────────────────────────────────────────────
	if ev.LiqSOL < a.cfg.MinLiqSOL {
		reject("insufficient_liquidity")
		return
	}

	// ── 4. Graduation guard (Pump.fun only) ───────────────────────────────────
	// Tokens ≥ 70 SOL are within 15 SOL of the migration threshold (~85 SOL).
	// They face a brief un-sellable window and price gap during migration, making
	// them unsuitable for short-hold paper trades.
	if ev.Source == "pump_fun" {
		solToGrad := pumpGraduationSOL - ev.LiqSOL
		if solToGrad < 0 {
			solToGrad = 0
		}
		meta.SolToGraduation = solToGrad
		if ev.LiqSOL >= pumpGraduationGuardSOL {
			reject("near_graduation")
			return
		}
	}

	// ── 5+6. Holder concentration + token metadata (parallel) ────────────────
	// Both are network-bound and independent; run concurrently to save ~200ms.
	type holderResult struct {
		rugged      bool
		topPct      float64
		count       int
		err         error
	}
	holderCh := make(chan holderResult, 1)
	symbolCh := make(chan string, 1)

	go func() {
		rugged, pct, cnt, e := a.isConcentrated(ctx, ev.Mint, mintInfo.Supply)
		holderCh <- holderResult{rugged, pct, cnt, e}
	}()
	go func() {
		if ev.Source == "pump_fun" {
			// Metaplex PDA is not written at Pump.fun token creation time.
			// Use the Pump.fun REST API instead (non-critical: blank on failure).
			symbolCh <- fetchPumpFunSymbol(ev.Mint.String())
		} else {
			symbolCh <- a.fetchSymbol(ctx, ev.Mint)
		}
	}()

	hr := <-holderCh
	symbol := <-symbolCh

	meta.TopHoldersPct = hr.topPct
	meta.HoldersCount = hr.count
	meta.Symbol = symbol

	if hr.err != nil {
		log.Warn().Err(hr.err).Str("mint", mintStr).Msg("analyzer: holder check failed")
	} else if hr.rugged {
		reject("rug_concentration")
		return
	}

	// ── 7. Vault balances + friction simulation ───────────────────────────────
	buyLamports := a.cfg.MaxBuyLamports()

	var (
		tokensOut      uint64
		frictionScore  float64
		priceImpactPct float64
		baseVault      solana.PublicKey
		quoteVault     solana.PublicKey
	)

	if ev.Source == "pump_fun" {
		// Pump.fun uses virtual reserves — no on-chain vault reads required.
		// All arithmetic is float64 to avoid uint64 overflow
		// (virtual_sol × virtual_tokens ≈ 3.2e25 > uint64 max of 1.84e19).
		tokensOut, frictionScore, priceImpactPct = pumpFunSimulate(ev.LiqSOL, buyLamports)

		// Add estimated Jito tip overhead so frictionScore reflects total execution
		// cost, not just swap loss. Mirrors executor.calcTipLamports(liqSOL, 0.001).
		// tipMultiplier = clamp(liqSOL/10, 1, 5); tip = 0.001 × multiplier SOL.
		tipMul := ev.LiqSOL / 10.0
		if tipMul < 1.0 {
			tipMul = 1.0
		} else if tipMul > 5.0 {
			tipMul = 5.0
		}
		// ×2: buy bundle tip + sell bundle tip are both paid in a real round-trip.
		tipPct := 2.0 * 0.001 * tipMul * 1e9 / float64(buyLamports) * 100
		frictionScore += tipPct

		baseVault = ev.Pool
		quoteVault = ev.Pool
	} else {
		// Raydium: read vault balances from on-chain pool state account.
		// The pool account may not be fully written at detection time — retry with
		// 300 ms backoff to handle the initialization race.
		var reserveBase, reserveQuote uint64
		const maxAttempts = 3
		for attempt := 0; attempt < maxAttempts; attempt++ {
			if attempt > 0 {
				select {
				case <-time.After(300 * time.Millisecond):
				case <-ctx.Done():
					reject("vault_read_error")
					return
				}
			}
			baseVault, quoteVault, err = a.resolveVaults(ctx, ev)
			if err != nil {
				continue
			}
			reserveBase, reserveQuote, err = a.readVaultBalances(ctx, baseVault, quoteVault)
			if err == nil {
				break
			}
		}
		if err != nil {
			log.Warn().Err(err).Str("mint", mintStr).Int("attempts", maxAttempts).Msg("analyzer: vault read failed after retries")
			reject("vault_read_error")
			return
		}
		frictionScore = simulateRoundTrip(reserveBase, reserveQuote, buyLamports)
		tokensOut, priceImpactPct = quoteEntry(reserveBase, reserveQuote, buyLamports)
	}

	metrics.FrictionScore.Set(frictionScore)
	metrics.EntryPriceImpact.Set(priceImpactPct)

	// Populate execution data before the friction check so the JSONL record is
	// complete even when a high_friction reject fires.
	var entryPriceSOL float64
	if tokensOut > 0 {
		entryPriceSOL = float64(buyLamports) / float64(tokensOut) / 1e9
	}
	execData = &dataset.Execution{
		EntryTokensRaw: fmt.Sprintf("%d", tokensOut),
		EntryPriceSOL:  entryPriceSOL,
		PriceImpactPct: priceImpactPct,
		FrictionScore:  frictionScore,
	}

	if a.cfg.MaxFrictionPct > 0 && frictionScore > a.cfg.MaxFrictionPct {
		reject("high_friction")
		return
	}

	// ── 8. Emit TradeSignal ───────────────────────────────────────────────────
	sig := &TradeSignal{
		Mint:                ev.Mint,
		Pool:                ev.Pool,
		Source:              ev.Source,
		LiqSOL:              ev.LiqSOL,
		FrictionScore:       frictionScore,
		TokensOut:           tokensOut,
		EntryPriceImpactPct: priceImpactPct,
		IsPumpFun:           ev.Source == "pump_fun",
		Symbol:              symbol,
		BaseVault:           baseVault,
		QuoteVault:          quoteVault,
		DetectedAt:          ev.DetectedAt,
	}

	log.Info().
		Str("mint", mintStr).
		Str("symbol", symbol).
		Str("source", ev.Source).
		Float64("liq_sol", ev.LiqSOL).
		Float64("friction_pct", frictionScore).
		Msg("analyzer: trade signal emitted")

	// Write PASSED_ANALYSIS record before sending to tradeCh — ensures positive
	// examples are captured in the dataset even if the executor crashes or drops
	// the position. Executor will write a second record with outcome + trajectory.
	if a.dw != nil {
		a.dw.Write(dataset.Record{
			ScannedAt:    ev.DetectedAt,
			TokenAddress: mintStr,
			Metadata:     meta,
			Execution:    execData,
			Trajectory:   []dataset.TrajectoryPoint{},
			FinalLabel:   "PASSED_ANALYSIS",
		})
	}

	select {
	case a.tradeCh <- sig:
	default:
		metrics.SignalsDropped.Inc()
		log.Warn().Str("mint", mintStr).Msg("analyzer: tradeCh full — dropping signal")
	}
}

// ─── Mint account ─────────────────────────────────────────────────────────────

// splMint is a minimal representation of the SPL Token Mint account layout.
// Layout (82 bytes, little-endian):
//
//	0-3:   MintAuthorityOption  (uint32; 0=None, 1=Some)
//	4-35:  MintAuthority        (32 bytes pubkey)
//	36-43: Supply               (uint64)
//	44:    Decimals             (uint8)
//	45:    IsInitialized        (bool)
//	46-49: FreezeAuthorityOption (uint32; 0=None, 1=Some)
//	50-81: FreezeAuthority      (32 bytes pubkey)
type splMint struct {
	MintAuthorityOption   uint32
	Supply                uint64
	Decimals              uint8
	FreezeAuthorityOption uint32
}

// getMintInfo fetches and manually decodes the SPL Token mint account.
func (a *Analyzer) getMintInfo(ctx context.Context, mint solana.PublicKey) (*splMint, error) {
	data, err := a.getAccountBytes(ctx, mint)
	if err != nil {
		return nil, err
	}
	if len(data) < 82 {
		return nil, fmt.Errorf("mint account too short: %d bytes", len(data))
	}
	m := &splMint{
		MintAuthorityOption:   binary.LittleEndian.Uint32(data[0:4]),
		Supply:                binary.LittleEndian.Uint64(data[36:44]),
		Decimals:              data[44],
		FreezeAuthorityOption: binary.LittleEndian.Uint32(data[46:50]),
	}
	return m, nil
}

// getAccountBytes fetches an account's raw data as bytes.
// Requests base64 encoding and decodes the result; works regardless of
// whether the rpc.DataBytesOrJSON type exposes GetBinaryData().
func (a *Analyzer) getAccountBytes(ctx context.Context, pubkey solana.PublicKey) ([]byte, error) {
	info, err := a.rpc.GetAccountInfoWithOpts(ctx, pubkey, &rpc.GetAccountInfoOpts{
		Encoding:   solana.EncodingBase64,
		Commitment: rpc.CommitmentConfirmed,
	})
	if err != nil {
		return nil, err
	}
	if info == nil || info.Value == nil || info.Value.Data == nil {
		return nil, fmt.Errorf("account not found: %s", pubkey)
	}
	// DataBytesOrJSON marshals to ["<base64data>","base64"] when encoding=base64.
	raw, err := json.Marshal(info.Value.Data)
	if err != nil {
		return nil, err
	}
	var parts []string
	if err := json.Unmarshal(raw, &parts); err != nil || len(parts) == 0 {
		return nil, fmt.Errorf("unexpected account data format: %s", raw)
	}
	return base64.StdEncoding.DecodeString(parts[0])
}

// ─── Holder concentration ─────────────────────────────────────────────────────

// isConcentrated returns whether the top-2 holders control > 80% of supply,
// the raw percentage, the number of large accounts returned, and any error.
// Returns (false, 0, 0, nil) on empty supply or missing data.
func (a *Analyzer) isConcentrated(ctx context.Context, mint solana.PublicKey, totalSupply uint64) (rugged bool, topPct float64, count int, err error) {
	if totalSupply == 0 {
		return false, 0, 0, nil
	}
	result, err := a.rpc.GetTokenLargestAccounts(ctx, mint, rpc.CommitmentConfirmed)
	if err != nil {
		return false, 0, 0, err
	}
	if result == nil || len(result.Value) == 0 {
		return false, 0, 0, nil
	}

	count = len(result.Value)
	top := result.Value
	n := 2
	if len(top) < n {
		n = len(top)
	}
	var topSum uint64
	for i := 0; i < n; i++ {
		amt, _ := strconv.ParseUint(top[i].Amount, 10, 64)
		topSum += amt
	}
	topPct = float64(topSum) / float64(totalSupply) * 100
	return topPct > 80, topPct, count, nil
}

// ─── Vault resolution (Raydium only) ─────────────────────────────────────────

// resolveVaults returns the base (token) and quote (WSOL) vault public keys
// from the Raydium AMM V4 pool state account.
// Only called for Raydium events; Pump.fun uses pumpFunSimulate instead.
func (a *Analyzer) resolveVaults(ctx context.Context, ev *listener.TokenEvent) (baseVault, quoteVault solana.PublicKey, err error) {
	// Raydium AMM V4: pool state account contains coinVaultKey and pcVaultKey.
	// Layout offsets (bytes):
	//   status:      0-7   (u64)
	//   nonce:       8     (u8)
	//   ...
	//   coinVault:   336-367 (pubkey)
	//   pcVault:     368-399 (pubkey)
	// Reference: https://github.com/raydium-io/raydium-amm/blob/master/program/src/state.rs
	data, err := a.getAccountBytes(ctx, ev.Pool)
	if err != nil {
		return solana.PublicKey{}, solana.PublicKey{}, err
	}
	if len(data) < 400 {
		return solana.PublicKey{}, solana.PublicKey{}, fmt.Errorf("pool state account too short: %d bytes (want ≥400)", len(data))
	}

	copy(baseVault[:], data[336:368])
	copy(quoteVault[:], data[368:400])
	return baseVault, quoteVault, nil
}

// ─── Vault balances (Raydium only) ───────────────────────────────────────────

// readVaultBalances returns the current token reserves for Raydium vault accounts.
// reserveBase is in raw token units; reserveQuote is in lamports.
// Pump.fun reserves are computed by pumpFunSimulate without any RPC calls.
func (a *Analyzer) readVaultBalances(ctx context.Context, baseVault, quoteVault solana.PublicKey) (reserveBase, reserveQuote uint64, err error) {
	baseResult, err := a.rpc.GetTokenAccountBalance(ctx, baseVault, rpc.CommitmentConfirmed)
	if err != nil {
		return 0, 0, err
	}
	quoteResult, err := a.rpc.GetTokenAccountBalance(ctx, quoteVault, rpc.CommitmentConfirmed)
	if err != nil {
		return 0, 0, err
	}

	baseAmt, _ := strconv.ParseUint(baseResult.Value.Amount, 10, 64)
	quoteAmt, _ := strconv.ParseUint(quoteResult.Value.Amount, 10, 64)
	return baseAmt, quoteAmt, nil
}

// ─── Pump.fun bonding curve simulation ───────────────────────────────────────

// pumpFunSimulate computes tokensOut, friction score, and price impact for a
// Pump.fun bonding curve buy, using the protocol's published virtual reserve
// constants. All arithmetic is float64 to avoid uint64 overflow
// (virtual_sol × virtual_tokens ≈ 3.2e25, which exceeds uint64 max of ~1.84e19).
//
// Protocol constants (from pump.fun source):
//
//	VIRTUAL_SOL_RESERVES   = 30 SOL = 30,000,000,000 lamports
//	VIRTUAL_TOKEN_RESERVES = 1,073,000,000,000,000 raw units (6 decimal places)
//	FEE_BASIS_POINTS       = 100 (1%)
func pumpFunSimulate(liqSOL float64, buyLamports uint64) (tokensOut uint64, frictionScore float64, priceImpactPct float64) {
	const (
		virtualSolF    = float64(30_000_000_000)        // 30 SOL in lamports
		virtualTokensF = float64(1_073_000_000_000_000) // protocol constant (6 decimals)
		feeMul         = float64(9900)                  // 100 - 100 basis points = 1% fee
		feeDen         = float64(10000)
	)

	// k is the constant product; real quote reserve = virtual + actual SOL in curve.
	k := virtualSolF * virtualTokensF
	rq := virtualSolF + liqSOL*1e9 // total lamports in quote reserve
	rb := k / rq                   // effective token reserve (decreases as curve fills)

	buy := float64(buyLamports)

	// Forward swap: SOL → tokens with 1% fee applied to input.
	inFee := buy * feeMul
	tOut := inFee * rb / (rq*feeDen + inFee)
	if tOut <= 0 {
		return 0, 100, 100
	}

	// Reverse swap: tokens → SOL on the post-buy reserves.
	rb2 := rb - tOut
	rq2 := rq + buy
	inFee2 := tOut * feeMul
	solOut := inFee2 * rq2 / (rb2*feeDen + inFee2)

	friction := (buy - solOut) / buy * 100
	if friction < 0 {
		friction = 0
	}

	// Price impact: (execution price − spot price) / spot price × 100.
	midPrice := rq / rb     // lamports per token unit (pre-trade spot)
	execPrice := buy / tOut // lamports per token unit (this execution)
	impact := (execPrice - midPrice) / midPrice * 100
	if impact < 0 {
		impact = 0
	}

	return uint64(tOut), friction, impact
}

// ─── Constant-product maths (Raydium) ────────────────────────────────────────

// cpAmountOut applies the constant-product formula with Raydium's 0.25% fee.
//
//	amountOut = (amountIn × 9975 × reserveOut) / (reserveIn × 10000 + amountIn × 9975)
func cpAmountOut(amountIn, reserveIn, reserveOut uint64) uint64 {
	if reserveIn == 0 || reserveOut == 0 {
		return 0
	}
	inWithFee := amountIn * 9975
	numerator := inWithFee * reserveOut
	denominator := reserveIn*10000 + inWithFee
	if denominator == 0 {
		return 0
	}
	return numerator / denominator
}

// simulateRoundTrip returns the percentage loss of a buy-then-sell round trip.
// amountIn is in lamports (SOL). Uses Raydium 0.25% fee.
func simulateRoundTrip(reserveBase, reserveQuote, amountInLamports uint64) float64 {
	if reserveBase == 0 || reserveQuote == 0 || amountInLamports == 0 {
		return 100
	}
	// Buy: lamports → tokens
	tokensReceived := cpAmountOut(amountInLamports, reserveQuote, reserveBase)
	if tokensReceived == 0 {
		return 100
	}
	// Sell: tokens → lamports (with updated reserves)
	newReserveBase := reserveBase - tokensReceived
	newReserveQuote := reserveQuote + amountInLamports
	solReceived := cpAmountOut(tokensReceived, newReserveBase, newReserveQuote)

	loss := float64(amountInLamports-solReceived) / float64(amountInLamports) * 100
	if loss < 0 {
		loss = 0
	}
	return loss
}

// quoteEntry returns (tokensOut, priceImpactPct) for a buy of amountInLamports.
// priceImpact = (executionPrice - midPrice) / midPrice * 100. Uses Raydium 0.25% fee.
func quoteEntry(reserveBase, reserveQuote, amountInLamports uint64) (tokensOut uint64, priceImpactPct float64) {
	tokensOut = cpAmountOut(amountInLamports, reserveQuote, reserveBase)
	if tokensOut == 0 || reserveBase == 0 || reserveQuote == 0 {
		return 0, 100
	}

	// Mid price: reserveQuote / reserveBase (lamports per token unit)
	midPrice := float64(reserveQuote) / float64(reserveBase)
	// Execution price: amountIn / tokensOut
	execPrice := float64(amountInLamports) / float64(tokensOut)
	if midPrice == 0 {
		return tokensOut, 100
	}
	priceImpactPct = (execPrice - midPrice) / midPrice * 100
	if priceImpactPct < 0 {
		priceImpactPct = 0
	}
	return tokensOut, priceImpactPct
}

// ─── Symbol fetch ─────────────────────────────────────────────────────────────

// fetchPumpFunSymbol calls the Pump.fun REST API to get the token symbol.
// Metaplex metadata PDA is not written at Pump.fun token creation time, so
// on-chain reads always fail for new tokens. The API is the reliable path.
// Returns empty string on any error — symbol is non-critical.
func fetchPumpFunSymbol(mint string) string {
	url := "https://frontend-api.pump.fun/coins/" + mint
	resp, err := pumpFunHTTPClient.Get(url)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return ""
	}
	var result struct {
		Symbol string `json:"symbol"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return ""
	}
	return strings.TrimSpace(result.Symbol)
}

// ─── Metaplex metadata ────────────────────────────────────────────────────────

// fetchSymbol attempts to read the token symbol from the Metaplex metadata PDA.
// Returns an empty string on any error (non-critical).
func (a *Analyzer) fetchSymbol(ctx context.Context, mint solana.PublicKey) string {
	metadataPDA, _, err := solana.FindProgramAddress(
		[][]byte{
			[]byte("metadata"),
			metaplexMetadataProg[:],
			mint[:],
		},
		metaplexMetadataProg,
	)
	if err != nil {
		return ""
	}

	data, err := a.getAccountBytes(ctx, metadataPDA)
	if err != nil {
		return ""
	}
	// Metaplex metadata layout (simplified):
	//   1 byte:  key (discriminator)
	//   32 bytes: update_authority
	//   32 bytes: mint
	//   4+N bytes: name (u32 length prefix + bytes)
	//   4+N bytes: symbol
	offset := 1 + 32 + 32
	if len(data) < offset+4 {
		return ""
	}
	// Skip name field.
	nameLen := int(binary.LittleEndian.Uint32(data[offset : offset+4]))
	offset += 4 + nameLen
	if len(data) < offset+4 {
		return ""
	}
	symLen := int(binary.LittleEndian.Uint32(data[offset : offset+4]))
	offset += 4
	if len(data) < offset+symLen {
		return ""
	}
	return string(data[offset : offset+symLen])
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func dexLabel(source string) string {
	switch source {
	case "raydium":
		return "Raydium_V4"
	case "pump_fun":
		return "Pump_Fun"
	default:
		return source
	}
}
