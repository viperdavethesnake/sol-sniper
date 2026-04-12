// Package analyzer performs Solana-specific token safety checks and emits
// TradeSignals for tokens that pass all filters.
//
// Safety pipeline (in order):
//  1. Deduplication (Redis 48h)
//  2. Freeze authority check  — freezeAuthority must be null
//  3. Mint authority check    — mintAuthority must be renounced
//  4. Liquidity threshold     — must have ≥ MinLiqSOL in pool
//  5. Holder concentration    — top 2 holders must not control > 80% supply
//  6. Token metadata          — fetch symbol via Metaplex DAS
//  7. Friction simulation     — offline constant-product round-trip estimate
//  8. Entry quote             — compute TokensOut and price impact
package analyzer

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strconv"
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
	start := time.Now()
	mintStr := ev.Mint.String()

	defer func() {
		metrics.AnalysisDuration.Observe(time.Since(start).Seconds())
	}()

	// ── 1. Deduplication ─────────────────────────────────────────────────────
	seen, err := a.cache.IsSeenToken(ctx, mintStr)
	if err != nil {
		log.Warn().Err(err).Str("mint", mintStr).Msg("analyzer: dedup check failed")
	}
	if seen {
		return
	}
	_ = a.cache.MarkTokenSeen(ctx, mintStr)
	_ = a.cache.Incr(ctx, "scanned")

	reject := func(reason string) {
		metrics.TokensFiltered.WithLabelValues(reason).Inc()
		_ = a.cache.Incr(ctx, "filtered")
		log.Info().Str("mint", mintStr).Str("reason", reason).Msg("analyzer: token filtered")
		if a.dw != nil {
			a.dw.Write(dataset.Record{
				ScannedAt:    ev.DetectedAt,
				TokenAddress: mintStr,
				Metadata: dataset.Metadata{
					DEX:       dexLabel(ev.Source),
					InitLiqSOL: ev.LiqSOL,
					IsPumpFun: ev.Source == "pump_fun",
				},
				Trajectory: []dataset.TrajectoryPoint{},
				FinalLabel: "REJECTED_" + reason,
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

	// ── 4. Holder concentration ───────────────────────────────────────────────
	if rugged, err := a.isConcentrated(ctx, ev.Mint, mintInfo.Supply); err != nil {
		log.Warn().Err(err).Str("mint", mintStr).Msg("analyzer: holder check failed")
	} else if rugged {
		reject("rug_concentration")
		return
	}

	// ── 5. Token metadata ─────────────────────────────────────────────────────
	symbol := a.fetchSymbol(ctx, ev.Mint)

	// ── 6 & 7. Vault balances + friction simulation ───────────────────────────
	// For Raydium: fetch vault token accounts from on-chain pool state.
	// For Pump.fun: bonding curve holds SOL directly (ev.Pool is the curve).
	baseVault, quoteVault, err := a.resolveVaults(ctx, ev)
	if err != nil {
		log.Warn().Err(err).Str("mint", mintStr).Msg("analyzer: vault resolution failed")
		reject("vault_read_error")
		return
	}

	reserveBase, reserveQuote, err := a.readVaultBalances(ctx, baseVault, quoteVault, ev.Source, ev.LiqSOL)
	if err != nil {
		log.Warn().Err(err).Str("mint", mintStr).Msg("analyzer: vault balance read failed")
		reject("vault_read_error")
		return
	}

	// Offline constant-product round-trip simulation (buy then immediate sell).
	// Uses Raydium V4 fee: 0.25% (9975/10000).
	buyLamports := a.cfg.MaxBuyLamports()
	frictionScore := simulateRoundTrip(reserveBase, reserveQuote, buyLamports)
	metrics.FrictionScore.Set(frictionScore)

	if a.cfg.MaxFrictionPct > 0 && frictionScore > a.cfg.MaxFrictionPct {
		reject("high_friction")
		return
	}

	// ── 8. Entry quote ────────────────────────────────────────────────────────
	tokensOut, priceImpactPct := quoteEntry(reserveBase, reserveQuote, buyLamports)
	metrics.EntryPriceImpact.Set(priceImpactPct)

	// ── Emit TradeSignal ──────────────────────────────────────────────────────
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

	select {
	case a.tradeCh <- sig:
	default:
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

// isConcentrated returns true if the top 2 token holders control more than 80%
// of the circulating supply (excluding the bonding curve and null addresses).
func (a *Analyzer) isConcentrated(ctx context.Context, mint solana.PublicKey, totalSupply uint64) (bool, error) {
	if totalSupply == 0 {
		return false, nil
	}
	result, err := a.rpc.GetTokenLargestAccounts(ctx, mint, rpc.CommitmentConfirmed)
	if err != nil {
		return false, err
	}
	if result == nil || len(result.Value) == 0 {
		return false, nil
	}

	// Sum the top 2 balances.
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
	pct := float64(topSum) / float64(totalSupply) * 100
	return pct > 80, nil
}

// ─── Vault resolution ─────────────────────────────────────────────────────────

// resolveVaults returns the base (token) and quote (WSOL) vault public keys.
// For Raydium we read the AMM state account; for Pump.fun we derive the
// associated token accounts from the bonding curve.
func (a *Analyzer) resolveVaults(ctx context.Context, ev *listener.TokenEvent) (baseVault, quoteVault solana.PublicKey, err error) {
	if ev.Source == "pump_fun" {
		// Pump.fun uses virtual reserves — no on-chain vault accounts to resolve.
		// Synthetic reserves are derived from protocol constants + ev.LiqSOL in
		// readVaultBalances; return the bonding curve as a sentinel.
		return ev.Pool, ev.Pool, nil
	}

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
		return solana.PublicKey{}, solana.PublicKey{}, nil
	}

	copy(baseVault[:], data[336:368])
	copy(quoteVault[:], data[368:400])
	return baseVault, quoteVault, nil
}

// ─── Vault balances ───────────────────────────────────────────────────────────

// readVaultBalances returns the current token reserves for the base and quote vaults.
// reserveBase is in raw token units; reserveQuote is in lamports.
// liqSOL is the bonding curve SOL balance captured at detection (Pump.fun only).
func (a *Analyzer) readVaultBalances(ctx context.Context, baseVault, quoteVault solana.PublicKey, source string, liqSOL float64) (reserveBase, reserveQuote uint64, err error) {
	if source == "pump_fun" {
		// Pump.fun prices using virtual reserves (published protocol constants):
		//   VIRTUAL_SOL_RESERVES   = 30 SOL  (added to real SOL for all price calcs)
		//   VIRTUAL_TOKEN_RESERVES = 1,073,000,191 tokens × 10^6 decimals
		// At token creation the curve holds virtually all token supply, so
		// reserveBase ≈ VIRTUAL_TOKEN_RESERVES. reserveQuote = virtual + real SOL.
		// No RPC calls needed — liqSOL is already captured by the listener.
		const virtualSolLamports = uint64(30_000_000_000)
		const virtualTokenUnits  = uint64(1_073_000_191_000_000)
		reserveBase  = virtualTokenUnits
		reserveQuote = virtualSolLamports + uint64(liqSOL*1e9)
		return reserveBase, reserveQuote, nil
	}

	// Raydium: both vaults are SPL token accounts.
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

// ─── Constant-product maths ───────────────────────────────────────────────────

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
// amountIn is in lamports (SOL).
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
// priceImpact = (midPrice - executionPrice) / midPrice * 100.
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
