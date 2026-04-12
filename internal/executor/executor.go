// Package executor opens and monitors positions on Solana.
// Every buy transaction is submitted as a Jito bundle (mandatory for MEV protection).
// Positions are tracked in memory and Redis; the monitor ticker checks TP/SL every
// MonitorInterval seconds.
//
// Jito bundles are submitted via the HTTP JSON-RPC API:
//
//	POST https://{JITO_BLOCK_ENGINE_URL}/api/v1/bundles
//	{"jsonrpc":"2.0","method":"sendBundle","params":[["<base64tx>"]]}
package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	associatedtokenaccount "github.com/gagliardetto/solana-go/programs/associated-token-account"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/rs/zerolog/log"

	"github.com/sol-sniper/bot/internal/analyzer"
	"github.com/sol-sniper/bot/internal/cache"
	"github.com/sol-sniper/bot/internal/config"
	"github.com/sol-sniper/bot/internal/dataset"
	"github.com/sol-sniper/bot/internal/metrics"
)

// Jito tip accounts (mainnet — rotate for load distribution).
var jitoTipAccounts = []solana.PublicKey{
	solana.MustPublicKeyFromBase58("96gYZGLnJYVFmbjzopPSU6QiEV5fGqZNyN9nmNhvrZU5"),
	solana.MustPublicKeyFromBase58("HFqU5x63VTqvB6pKYRS6vnmoUFxeFXZyoXnJKY5nh35k"),
	solana.MustPublicKeyFromBase58("Cw8CFyM9FkoMi7K7Crf6HNQqf4uEMzpKw6QNghXLvLkY"),
	solana.MustPublicKeyFromBase58("ADaUMid9yfUytqMBgopwjb2DTLSokTSzL1owiUQMKmzq"),
}

// Raydium AMM V4 swapBaseIn instruction discriminator.
const raydiumSwapDiscriminator byte = 9

// Pump.fun buy/sell Anchor instruction discriminators.
var pumpFunBuyDiscriminator = [8]byte{0x66, 0x06, 0x3d, 0x12, 0x01, 0xda, 0xeb, 0xea}
var pumpFunSellDiscriminator = [8]byte{0x33, 0xe6, 0x85, 0xa4, 0x01, 0x7f, 0x83, 0xad}

// Pump.fun mainnet fee recipient.
var pumpFunFeeRecipient = solana.MustPublicKeyFromBase58("CebN5WGQ4jvEPvsVU4EoHEpgznyQHePKCo5iu87i3xdK")

// Position represents an open trade.
type Position struct {
	Token          string     `json:"token"`    // base58 mint
	Pool           string     `json:"pool"`     // base58 pool/curve
	Source         string     `json:"source"`   // "raydium" | "pump_fun"
	Symbol         string     `json:"symbol"`
	BaseVault      string     `json:"base_vault"`
	QuoteVault     string     `json:"quote_vault"`
	LiqSOL         float64    `json:"liq_sol"`
	FrictionScore  float64    `json:"friction_score"`
	PriceImpactPct float64    `json:"price_impact_pct"`
	BuySignature   string     `json:"buy_signature"`
	SOLSpent       float64    `json:"sol_spent"`
	TipSOL         float64    `json:"tip_sol"`
	TokensIn       uint64     `json:"tokens_in"`
	OpenedAt       time.Time  `json:"opened_at"`
	Status         string     `json:"status"` // "open" | "sold" | "failed"
	SellSignature  string     `json:"sell_signature,omitempty"`
	ClosedAt       *time.Time `json:"closed_at,omitempty"`
	// Set by monitor before sell is triggered; used for dataset write.
	lastQuotedSOL float64
	snaps         map[string]bool
}

// Executor manages position lifecycle.
type Executor struct {
	cfg    *config.Config
	rpc    *rpc.Client
	cache  *cache.Client
	dw     *dataset.Writer
	wallet solana.PrivateKey

	mu        sync.RWMutex
	positions map[string]*Position // keyed by base58 mint

	jitoHTTPURL string
	httpClient  *http.Client

	// Rolling tip-success counters for the JitoTipSuccessRatio metric.
	tipAttempts int64
	tipLanded   int64
}

// New creates an Executor. Returns error if the private key is invalid.
func New(cfg *config.Config, c *cache.Client, dw *dataset.Writer) (*Executor, error) {
	privKey, err := solana.PrivateKeyFromBase58(cfg.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("executor: invalid private key: %w", err)
	}

	return &Executor{
		cfg:         cfg,
		rpc:         rpc.New(cfg.RPCURL),
		cache:       c,
		dw:          dw,
		wallet:      privKey,
		positions:   make(map[string]*Position),
		jitoHTTPURL: fmt.Sprintf("https://%s/api/v1/bundles", cfg.JitoURL),
		httpClient:  &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// Run reads TradeSignals and spawns one goroutine per signal.
func (e *Executor) Run(ctx context.Context, tradeCh <-chan *analyzer.TradeSignal) {
	for {
		select {
		case <-ctx.Done():
			return
		case sig, ok := <-tradeCh:
			if !ok {
				return
			}
			go e.executeBuy(ctx, sig)
		}
	}
}

// MonitorPositions checks all open positions on a ticker for TP/SL triggers.
func (e *Executor) MonitorPositions(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(e.cfg.MonitorInterval) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.checkAllPositions(ctx)
		}
	}
}

// ActivePositions returns a snapshot of the current in-memory positions map.
func (e *Executor) ActivePositions() map[string]*Position {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make(map[string]*Position, len(e.positions))
	for k, v := range e.positions {
		out[k] = v
	}
	return out
}

// KillSwitch sells all open positions and arms the Redis kill switch.
func (e *Executor) KillSwitch(ctx context.Context) error {
	if err := e.cache.SetKillSwitch(ctx, true); err != nil {
		return err
	}
	e.mu.RLock()
	keys := make([]string, 0, len(e.positions))
	for k := range e.positions {
		keys = append(keys, k)
	}
	e.mu.RUnlock()

	for _, k := range keys {
		e.mu.RLock()
		pos := e.positions[k]
		e.mu.RUnlock()
		if pos != nil && pos.Status == "open" {
			go e.executeSell(ctx, pos, "kill_switch")
		}
	}
	return nil
}

// DisarmKillSwitch re-enables buying.
func (e *Executor) DisarmKillSwitch(ctx context.Context) error {
	return e.cache.SetKillSwitch(ctx, false)
}

// ─── Buy ──────────────────────────────────────────────────────────────────────

func (e *Executor) executeBuy(ctx context.Context, sig *analyzer.TradeSignal) {
	start := time.Now()
	mintStr := sig.Mint.String()

	if active, _ := e.cache.KillSwitchActive(ctx); active {
		return
	}

	e.mu.RLock()
	_, exists := e.positions[mintStr]
	e.mu.RUnlock()
	if exists {
		return
	}

	buyLamports := e.cfg.MaxBuyLamports()
	tipLamports := calcTipLamports(sig.LiqSOL, 0.001)
	tipSOL := float64(tipLamports) / 1e9

	swapIx, err := e.buildSwapInstruction(ctx, sig, buyLamports)
	if err != nil {
		log.Error().Err(err).Str("mint", mintStr).Msg("executor: failed to build swap instruction")
		return
	}

	tipAccount := jitoTipAccounts[int(time.Now().UnixNano())%len(jitoTipAccounts)]
	tipIx, err := system.NewTransferInstruction(tipLamports, e.wallet.PublicKey(), tipAccount).ValidateAndBuild()
	if err != nil {
		log.Error().Err(err).Msg("executor: failed to build tip instruction")
		return
	}

	bhResp, err := e.rpc.GetLatestBlockhash(ctx, rpc.CommitmentConfirmed)
	if err != nil {
		log.Error().Err(err).Msg("executor: failed to get blockhash")
		return
	}

	tx, err := solana.NewTransaction(
		[]solana.Instruction{swapIx, tipIx},
		bhResp.Value.Blockhash,
		solana.TransactionPayer(e.wallet.PublicKey()),
	)
	if err != nil {
		log.Error().Err(err).Str("mint", mintStr).Msg("executor: failed to build transaction")
		return
	}
	if _, err := tx.Sign(func(pk solana.PublicKey) *solana.PrivateKey {
		if pk == e.wallet.PublicKey() {
			return &e.wallet
		}
		return nil
	}); err != nil {
		log.Error().Err(err).Str("mint", mintStr).Msg("executor: failed to sign transaction")
		return
	}

	metrics.SnipesAttempted.Inc()
	_ = e.cache.Incr(ctx, "snipes_attempted")
	metrics.JitoTipSOL.Set(tipSOL)
	metrics.ExecutionDuration.Observe(time.Since(start).Seconds())

	if e.cfg.SimulationMode {
		log.Info().
			Str("mint", mintStr).
			Float64("buy_sol", float64(buyLamports)/1e9).
			Float64("tip_sol", tipSOL).
			Msg("executor: [SIM] bundle skipped — simulation mode")
		e.savePosition(ctx, sig, "", float64(buyLamports)/1e9, tipSOL, sig.TokensOut)
		return
	}

	txSig, landed, err := e.submitBundle(ctx, tx)
	e.tipAttempts++
	if landed {
		e.tipLanded++
	}
	if e.tipAttempts > 0 {
		metrics.JitoTipSuccessRatio.Set(float64(e.tipLanded) / float64(e.tipAttempts))
	}

	if err != nil {
		log.Error().Err(err).Str("mint", mintStr).Msg("executor: bundle submission failed")
		return
	}
	if !landed {
		log.Warn().Str("mint", mintStr).Str("sig", txSig).Msg("executor: bundle did not land")
		return
	}

	log.Info().
		Str("mint", mintStr).
		Str("sig", txSig).
		Float64("tip_sol", tipSOL).
		Msg("executor: buy bundle landed")

	metrics.SnipesSuccessful.Inc()
	_ = e.cache.Incr(ctx, "snipes_successful")
	e.savePosition(ctx, sig, txSig, float64(buyLamports)/1e9, tipSOL, sig.TokensOut)
}

func (e *Executor) savePosition(ctx context.Context, sig *analyzer.TradeSignal, txSig string, solSpent, tipSOL float64, tokensIn uint64) {
	mintStr := sig.Mint.String()
	pos := &Position{
		Token:          mintStr,
		Pool:           sig.Pool.String(),
		Source:         sig.Source,
		Symbol:         sig.Symbol,
		BaseVault:      sig.BaseVault.String(),
		QuoteVault:     sig.QuoteVault.String(),
		LiqSOL:         sig.LiqSOL,
		FrictionScore:  sig.FrictionScore,
		PriceImpactPct: sig.EntryPriceImpactPct,
		BuySignature:   txSig,
		SOLSpent:       solSpent,
		TipSOL:         tipSOL,
		TokensIn:       tokensIn,
		OpenedAt:       time.Now(),
		Status:         "open",
		snaps:          make(map[string]bool),
	}
	e.mu.Lock()
	e.positions[mintStr] = pos
	e.mu.Unlock()
	_ = e.cache.SavePosition(ctx, mintStr, pos)
	metrics.ActivePositions.Inc()
}

// ─── Sell ─────────────────────────────────────────────────────────────────────

func (e *Executor) executeSell(ctx context.Context, pos *Position, trigger string) {
	mintStr := pos.Token
	sig := &analyzer.TradeSignal{
		Mint:       solana.MustPublicKeyFromBase58(mintStr),
		Pool:       solana.MustPublicKeyFromBase58(pos.Pool),
		Source:     pos.Source,
		BaseVault:  solana.MustPublicKeyFromBase58(pos.BaseVault),
		QuoteVault: solana.MustPublicKeyFromBase58(pos.QuoteVault),
	}

	sellIx, err := e.buildSellInstruction(ctx, sig, pos.TokensIn)
	if err != nil {
		log.Error().Err(err).Str("mint", mintStr).Msg("executor: failed to build sell instruction")
		return
	}

	tipLamports := uint64(0.0005 * 1e9)
	tipAccount := jitoTipAccounts[int(time.Now().UnixNano())%len(jitoTipAccounts)]
	tipIx, err := system.NewTransferInstruction(tipLamports, e.wallet.PublicKey(), tipAccount).ValidateAndBuild()
	if err != nil {
		log.Error().Err(err).Msg("executor: sell tip instruction failed")
		return
	}

	bhResp, err := e.rpc.GetLatestBlockhash(ctx, rpc.CommitmentConfirmed)
	if err != nil {
		log.Error().Err(err).Msg("executor: blockhash fetch failed for sell")
		return
	}

	tx, err := solana.NewTransaction(
		[]solana.Instruction{sellIx, tipIx},
		bhResp.Value.Blockhash,
		solana.TransactionPayer(e.wallet.PublicKey()),
	)
	if err != nil {
		log.Error().Err(err).Str("mint", mintStr).Msg("executor: failed to build sell tx")
		return
	}
	if _, err := tx.Sign(func(pk solana.PublicKey) *solana.PrivateKey {
		if pk == e.wallet.PublicKey() {
			return &e.wallet
		}
		return nil
	}); err != nil {
		log.Error().Err(err).Str("mint", mintStr).Msg("executor: failed to sign sell tx")
		return
	}

	metrics.SellsExecuted.WithLabelValues(trigger).Inc()

	if e.cfg.SimulationMode {
		log.Info().Str("mint", mintStr).Str("trigger", trigger).Msg("executor: [SIM] sell skipped")
		e.closePosition(ctx, pos, trigger, "")
		return
	}

	txSig, landed, err := e.submitBundle(ctx, tx)
	if err != nil || !landed {
		log.Error().Err(err).Str("mint", mintStr).Msg("executor: sell bundle failed")
		return
	}

	log.Info().
		Str("mint", mintStr).
		Str("trigger", trigger).
		Str("sig", txSig).
		Float64("pnl_sol", pos.lastQuotedSOL-pos.SOLSpent).
		Msg("executor: sell landed")

	e.closePosition(ctx, pos, trigger, txSig)

	if trigger == "stop_loss" {
		_ = e.cache.BlacklistDeployer(ctx, pos.Pool)
	}
}

func (e *Executor) closePosition(ctx context.Context, pos *Position, trigger, txSig string) {
	now := time.Now()
	deltaSOL := pos.lastQuotedSOL - pos.SOLSpent
	pos.Status = "sold"
	pos.SellSignature = txSig
	pos.ClosedAt = &now

	e.mu.Lock()
	delete(e.positions, pos.Token)
	e.mu.Unlock()

	_ = e.cache.DeletePosition(ctx, pos.Token)
	_ = e.cache.IncrPnL(ctx, deltaSOL)
	_ = e.cache.AddToLeaderboard(ctx, cache.LeaderboardEntry{
		Token:    pos.Token,
		Symbol:   pos.Symbol,
		DeltaSOL: deltaSOL,
		Trigger:  trigger,
		ClosedAt: now,
	})
	metrics.ActivePositions.Dec()
	metrics.PnLSOL.Add(deltaSOL)

	if e.dw != nil {
		traj := e.buildTrajectory(ctx, pos.Token)
		e.dw.Write(dataset.Record{
			ScannedAt:    pos.OpenedAt,
			TokenAddress: pos.Token,
			Metadata: dataset.Metadata{
				DEX:            dexLabel(pos.Source),
				InitLiqSOL:     pos.LiqSOL,
				TaxPct:         pos.FrictionScore,
				IsRenounced:    true,
				Symbol:         pos.Symbol,
				IsPumpFun:      pos.Source == "pump_fun",
				PriorityTipSOL: pos.TipSOL,
				BundleLanded:   pos.BuySignature != "",
			},
			Execution: &dataset.Execution{
				EntryTokensRaw: strconv.FormatUint(pos.TokensIn, 10),
				EntryPriceSOL:  safeDivide(pos.SOLSpent, float64(pos.TokensIn)),
				PriceImpactPct: pos.PriceImpactPct,
				FrictionScore:  pos.FrictionScore,
			},
			Trajectory: traj,
			FinalLabel: strings.ToUpper(trigger),
		})
	}
}

// ─── Position monitor ─────────────────────────────────────────────────────────

func (e *Executor) checkAllPositions(ctx context.Context) {
	if active, _ := e.cache.KillSwitchActive(ctx); active {
		return
	}
	e.mu.RLock()
	keys := make([]string, 0, len(e.positions))
	for k := range e.positions {
		keys = append(keys, k)
	}
	e.mu.RUnlock()

	for _, k := range keys {
		e.mu.RLock()
		pos := e.positions[k]
		e.mu.RUnlock()
		if pos != nil && pos.Status == "open" {
			e.checkPosition(ctx, pos)
		}
	}
}

var milestones = []struct {
	label    string
	duration time.Duration
}{
	{"10s", 10 * time.Second},
	{"30s", 30 * time.Second},
	{"1m", time.Minute},
	{"2m", 2 * time.Minute},
	{"5m", 5 * time.Minute},
	{"10m", 10 * time.Minute},
}

func (e *Executor) checkPosition(ctx context.Context, pos *Position) {
	baseVault := solana.MustPublicKeyFromBase58(pos.BaseVault)
	quoteVault := solana.MustPublicKeyFromBase58(pos.QuoteVault)

	reserveBase, reserveQuote, err := e.readVaultBalances(ctx, baseVault, quoteVault, pos.Source)
	if err != nil {
		log.Warn().Err(err).Str("mint", pos.Token).Msg("executor: monitor vault read failed")
		return
	}

	currentSOL := float64(cpAmountOut(pos.TokensIn, reserveBase, reserveQuote)) / 1e9
	pos.lastQuotedSOL = currentSOL
	roiPct := (currentSOL - pos.SOLSpent) / pos.SOLSpent * 100
	elapsed := time.Since(pos.OpenedAt)

	for _, m := range milestones {
		if elapsed >= m.duration && !pos.snaps[m.label] {
			pos.snaps[m.label] = true
			_ = e.cache.AppendTrajectorySnap(ctx, pos.Token, cache.TrajectorySnap{
				Label:      m.label,
				ElapsedS:   int64(elapsed.Seconds()),
				ROIPct:     roiPct,
				CurrentSOL: currentSOL,
				RecordedAt: time.Now(),
			})
		}
	}

	switch {
	case roiPct >= e.cfg.TakeProfitPct:
		log.Info().Str("mint", pos.Token).Float64("roi_pct", roiPct).Msg("executor: take-profit")
		go e.executeSell(ctx, pos, "take_profit")
	case roiPct <= -e.cfg.StopLossPct:
		log.Info().Str("mint", pos.Token).Float64("roi_pct", roiPct).Msg("executor: stop-loss")
		go e.executeSell(ctx, pos, "stop_loss")
	case elapsed > 15*time.Minute:
		log.Info().Str("mint", pos.Token).Msg("executor: position timeout")
		go e.executeSell(ctx, pos, "timeout")
	}
}

func (e *Executor) buildTrajectory(ctx context.Context, tokenAddr string) []dataset.TrajectoryPoint {
	snaps, err := e.cache.GetTrajectory(ctx, tokenAddr)
	if err != nil || len(snaps) == 0 {
		return []dataset.TrajectoryPoint{}
	}
	out := make([]dataset.TrajectoryPoint, len(snaps))
	for i, s := range snaps {
		out[i] = dataset.TrajectoryPoint{
			TPlus: int(s.ElapsedS),
			ROI:   1 + s.ROIPct/100,
		}
	}
	return out
}

// ─── Jito HTTP bundle submission ──────────────────────────────────────────────

// submitBundle base64-encodes the transaction and POSTs it to the Jito HTTP API.
// Returns (signature, landed, error).
func (e *Executor) submitBundle(ctx context.Context, tx *solana.Transaction) (string, bool, error) {
	txBytes, err := tx.MarshalBinary()
	if err != nil {
		return "", false, err
	}
	txB64 := base64.StdEncoding.EncodeToString(txBytes)
	sig := tx.Signatures[0].String()

	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "sendBundle",
		"params":  []any{[]string{txB64}, map[string]string{"encoding": "base64"}},
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.jitoHTTPURL, bytes.NewReader(body))
	if err != nil {
		return sig, false, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return sig, false, fmt.Errorf("jito HTTP: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return sig, false, fmt.Errorf("jito HTTP %d: %s", resp.StatusCode, b)
	}

	// Poll for confirmation (up to 60 s).
	landed, err := e.awaitConfirmation(ctx, tx.Signatures[0], 60*time.Second)
	return sig, landed, err
}

// awaitConfirmation polls GetSignatureStatuses until confirmed or timeout.
func (e *Executor) awaitConfirmation(ctx context.Context, sig solana.Signature, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(2 * time.Second):
		}
		result, err := e.rpc.GetSignatureStatuses(ctx, false, sig)
		if err != nil {
			continue
		}
		if len(result.Value) > 0 && result.Value[0] != nil {
			if result.Value[0].Err != nil {
				return false, fmt.Errorf("transaction failed: %v", result.Value[0].Err)
			}
			st := result.Value[0].ConfirmationStatus
			if st == rpc.ConfirmationStatusConfirmed || st == rpc.ConfirmationStatusFinalized {
				return true, nil
			}
		}
	}
	return false, nil
}

// ─── Instruction builders ─────────────────────────────────────────────────────

func (e *Executor) buildSwapInstruction(ctx context.Context, sig *analyzer.TradeSignal, amountInLamports uint64) (solana.Instruction, error) {
	if sig.Source == "pump_fun" {
		return e.buildPumpFunBuy(sig, amountInLamports)
	}
	return e.buildRaydiumSwap(ctx, sig, amountInLamports)
}

func (e *Executor) buildSellInstruction(ctx context.Context, sig *analyzer.TradeSignal, tokenAmount uint64) (solana.Instruction, error) {
	if sig.Source == "pump_fun" {
		return e.buildPumpFunSell(sig, tokenAmount)
	}
	return e.buildRaydiumSell(ctx, sig, tokenAmount)
}

// buildRaydiumSwap constructs a Raydium AMM V4 swapBaseIn instruction (SOL→token).
// Instruction data: [discriminator u8][amountIn u64 LE][minAmountOut u64 LE]
func (e *Executor) buildRaydiumSwap(ctx context.Context, sig *analyzer.TradeSignal, amountInLamports uint64) (solana.Instruction, error) {
	minOut := uint64(float64(sig.TokensOut) * (1 - e.cfg.SlippagePct/100))

	data := make([]byte, 17)
	data[0] = raydiumSwapDiscriminator
	binary.LittleEndian.PutUint64(data[1:9], amountInLamports)
	binary.LittleEndian.PutUint64(data[9:17], minOut)

	wsol := solana.MustPublicKeyFromBase58("So11111111111111111111111111111111111111112")
	userWSOL, _, err := solana.FindAssociatedTokenAddress(e.wallet.PublicKey(), wsol)
	if err != nil {
		return nil, err
	}
	userTokenAcc, _, err := solana.FindAssociatedTokenAddress(e.wallet.PublicKey(), sig.Mint)
	if err != nil {
		return nil, err
	}

	raydiumProg := solana.MustPublicKeyFromBase58(e.cfg.RaydiumAMMProgram)
	ammAuthority, _, _ := solana.FindProgramAddress([][]byte{[]byte("amm authority")}, raydiumProg)
	openOrders, serumProg, serumMarket, serumBids, serumAsks, serumEventQ, serumCoinVault, serumPCVault, serumVaultSigner, err := e.fetchRaydiumPoolAccounts(ctx, sig.Pool)
	if err != nil {
		return nil, fmt.Errorf("fetch pool accounts: %w", err)
	}

	accounts := solana.AccountMetaSlice{
		{PublicKey: solana.TokenProgramID, IsSigner: false, IsWritable: false},
		{PublicKey: sig.Pool, IsSigner: false, IsWritable: true},
		{PublicKey: ammAuthority, IsSigner: false, IsWritable: false},
		{PublicKey: openOrders, IsSigner: false, IsWritable: true},
		{PublicKey: sig.QuoteVault, IsSigner: false, IsWritable: true}, // pc_vault (WSOL)
		{PublicKey: sig.BaseVault, IsSigner: false, IsWritable: true},  // coin_vault (token)
		{PublicKey: serumProg, IsSigner: false, IsWritable: false},
		{PublicKey: serumMarket, IsSigner: false, IsWritable: true},
		{PublicKey: serumBids, IsSigner: false, IsWritable: true},
		{PublicKey: serumAsks, IsSigner: false, IsWritable: true},
		{PublicKey: serumEventQ, IsSigner: false, IsWritable: true},
		{PublicKey: serumCoinVault, IsSigner: false, IsWritable: true},
		{PublicKey: serumPCVault, IsSigner: false, IsWritable: true},
		{PublicKey: serumVaultSigner, IsSigner: false, IsWritable: false},
		{PublicKey: userWSOL, IsSigner: false, IsWritable: true},
		{PublicKey: userTokenAcc, IsSigner: false, IsWritable: true},
		{PublicKey: e.wallet.PublicKey(), IsSigner: true, IsWritable: false},
	}
	return &rawInstruction{programID: raydiumProg, accounts: accounts, data: data}, nil
}

// buildRaydiumSell constructs a Raydium AMM V4 swapBaseIn instruction (token→SOL).
func (e *Executor) buildRaydiumSell(ctx context.Context, sig *analyzer.TradeSignal, tokenAmount uint64) (solana.Instruction, error) {
	data := make([]byte, 17)
	data[0] = raydiumSwapDiscriminator
	binary.LittleEndian.PutUint64(data[1:9], tokenAmount)
	binary.LittleEndian.PutUint64(data[9:17], 0) // minAmountOut = 0 (accept any)

	wsol := solana.MustPublicKeyFromBase58("So11111111111111111111111111111111111111112")
	userWSOL, _, err := solana.FindAssociatedTokenAddress(e.wallet.PublicKey(), wsol)
	if err != nil {
		return nil, err
	}
	userTokenAcc, _, err := solana.FindAssociatedTokenAddress(e.wallet.PublicKey(), sig.Mint)
	if err != nil {
		return nil, err
	}

	raydiumProg := solana.MustPublicKeyFromBase58(e.cfg.RaydiumAMMProgram)
	ammAuthority, _, _ := solana.FindProgramAddress([][]byte{[]byte("amm authority")}, raydiumProg)
	openOrders, serumProg, serumMarket, serumBids, serumAsks, serumEventQ, serumCoinVault, serumPCVault, serumVaultSigner, err := e.fetchRaydiumPoolAccounts(ctx, sig.Pool)
	if err != nil {
		return nil, fmt.Errorf("fetch pool accounts (sell): %w", err)
	}

	accounts := solana.AccountMetaSlice{
		{PublicKey: solana.TokenProgramID, IsSigner: false, IsWritable: false},
		{PublicKey: sig.Pool, IsSigner: false, IsWritable: true},
		{PublicKey: ammAuthority, IsSigner: false, IsWritable: false},
		{PublicKey: openOrders, IsSigner: false, IsWritable: true},
		{PublicKey: sig.BaseVault, IsSigner: false, IsWritable: true},  // coin_vault (token) — source
		{PublicKey: sig.QuoteVault, IsSigner: false, IsWritable: true}, // pc_vault (WSOL) — dest
		{PublicKey: serumProg, IsSigner: false, IsWritable: false},
		{PublicKey: serumMarket, IsSigner: false, IsWritable: true},
		{PublicKey: serumBids, IsSigner: false, IsWritable: true},
		{PublicKey: serumAsks, IsSigner: false, IsWritable: true},
		{PublicKey: serumEventQ, IsSigner: false, IsWritable: true},
		{PublicKey: serumCoinVault, IsSigner: false, IsWritable: true},
		{PublicKey: serumPCVault, IsSigner: false, IsWritable: true},
		{PublicKey: serumVaultSigner, IsSigner: false, IsWritable: false},
		{PublicKey: userTokenAcc, IsSigner: false, IsWritable: true},
		{PublicKey: userWSOL, IsSigner: false, IsWritable: true},
		{PublicKey: e.wallet.PublicKey(), IsSigner: true, IsWritable: false},
	}
	return &rawInstruction{programID: raydiumProg, accounts: accounts, data: data}, nil
}

// buildPumpFunBuy constructs a Pump.fun buy instruction.
// data: [discriminator 8b][tokenAmount u64 LE][maxSolCost u64 LE]
func (e *Executor) buildPumpFunBuy(sig *analyzer.TradeSignal, amountInLamports uint64) (solana.Instruction, error) {
	userTokenAcc, _, err := solana.FindAssociatedTokenAddress(e.wallet.PublicKey(), sig.Mint)
	if err != nil {
		return nil, err
	}

	pumpProg := solana.MustPublicKeyFromBase58(e.cfg.PumpFunProgram)
	globalPDA, _, _ := solana.FindProgramAddress([][]byte{[]byte("global")}, pumpProg)
	eventAuth, _, _ := solana.FindProgramAddress([][]byte{[]byte("__event_authority")}, pumpProg)

	data := make([]byte, 24)
	copy(data[:8], pumpFunBuyDiscriminator[:])
	binary.LittleEndian.PutUint64(data[8:16], sig.TokensOut)
	binary.LittleEndian.PutUint64(data[16:24], amountInLamports)

	accounts := solana.AccountMetaSlice{
		{PublicKey: globalPDA, IsSigner: false, IsWritable: false},
		{PublicKey: pumpFunFeeRecipient, IsSigner: false, IsWritable: true},
		{PublicKey: sig.Mint, IsSigner: false, IsWritable: false},
		{PublicKey: sig.Pool, IsSigner: false, IsWritable: true},     // bonding_curve
		{PublicKey: sig.BaseVault, IsSigner: false, IsWritable: true}, // associated_bonding_curve (token ATA)
		{PublicKey: userTokenAcc, IsSigner: false, IsWritable: true},
		{PublicKey: e.wallet.PublicKey(), IsSigner: true, IsWritable: true},
		{PublicKey: solana.SystemProgramID, IsSigner: false, IsWritable: false},
		{PublicKey: solana.TokenProgramID, IsSigner: false, IsWritable: false},
		{PublicKey: solana.MustPublicKeyFromBase58("SysvarRent111111111111111111111111111111111"), IsSigner: false, IsWritable: false},
		{PublicKey: associatedtokenaccount.ProgramID, IsSigner: false, IsWritable: false},
		{PublicKey: eventAuth, IsSigner: false, IsWritable: false},
		{PublicKey: pumpProg, IsSigner: false, IsWritable: false},
	}
	return &rawInstruction{programID: pumpProg, accounts: accounts, data: data}, nil
}

// buildPumpFunSell constructs a Pump.fun sell instruction.
func (e *Executor) buildPumpFunSell(sig *analyzer.TradeSignal, tokenAmount uint64) (solana.Instruction, error) {
	userTokenAcc, _, err := solana.FindAssociatedTokenAddress(e.wallet.PublicKey(), sig.Mint)
	if err != nil {
		return nil, err
	}

	pumpProg := solana.MustPublicKeyFromBase58(e.cfg.PumpFunProgram)
	globalPDA, _, _ := solana.FindProgramAddress([][]byte{[]byte("global")}, pumpProg)
	eventAuth, _, _ := solana.FindProgramAddress([][]byte{[]byte("__event_authority")}, pumpProg)

	data := make([]byte, 24)
	copy(data[:8], pumpFunSellDiscriminator[:])
	binary.LittleEndian.PutUint64(data[8:16], tokenAmount)
	binary.LittleEndian.PutUint64(data[16:24], 0) // minSolOutput = 0

	accounts := solana.AccountMetaSlice{
		{PublicKey: globalPDA, IsSigner: false, IsWritable: false},
		{PublicKey: pumpFunFeeRecipient, IsSigner: false, IsWritable: true},
		{PublicKey: sig.Mint, IsSigner: false, IsWritable: false},
		{PublicKey: sig.Pool, IsSigner: false, IsWritable: true},
		{PublicKey: sig.BaseVault, IsSigner: false, IsWritable: true},
		{PublicKey: userTokenAcc, IsSigner: false, IsWritable: true},
		{PublicKey: e.wallet.PublicKey(), IsSigner: true, IsWritable: true},
		{PublicKey: solana.SystemProgramID, IsSigner: false, IsWritable: false},
		{PublicKey: associatedtokenaccount.ProgramID, IsSigner: false, IsWritable: false},
		{PublicKey: solana.TokenProgramID, IsSigner: false, IsWritable: false},
		{PublicKey: eventAuth, IsSigner: false, IsWritable: false},
		{PublicKey: pumpProg, IsSigner: false, IsWritable: false},
	}
	return &rawInstruction{programID: pumpProg, accounts: accounts, data: data}, nil
}

// ─── Raydium pool state reader ────────────────────────────────────────────────

// fetchRaydiumPoolAccounts reads Raydium AMM V4 state to extract Serum/OpenBook
// market accounts needed for the swap instruction.
//
// AMM V4 state byte offsets (from raydium-amm program source):
//   280: open_orders     (32)
//   312: serum_program   (32)
//   344: serum_market    (32)
//   376: serum_bids      (32)
//   408: serum_asks      (32)
//   440: serum_event_q   (32)
//   472: serum_coin_vault(32)
//   504: serum_pc_vault  (32)
//   536: serum_vault_sign(32)
func (e *Executor) fetchRaydiumPoolAccounts(ctx context.Context, ammID solana.PublicKey) (
	openOrders, serumProg, serumMarket, serumBids, serumAsks, serumEventQ, serumCoinVault, serumPCVault, serumVaultSigner solana.PublicKey, err error,
) {
	data, err := getRPCAccountBytes(ctx, e.rpc, ammID)
	if err != nil {
		return
	}
	if len(data) < 568 {
		err = fmt.Errorf("AMM state too short: %d bytes", len(data))
		return
	}
	copy(openOrders[:], data[280:312])
	copy(serumProg[:], data[312:344])
	copy(serumMarket[:], data[344:376])
	copy(serumBids[:], data[376:408])
	copy(serumAsks[:], data[408:440])
	copy(serumEventQ[:], data[440:472])
	copy(serumCoinVault[:], data[472:504])
	copy(serumPCVault[:], data[504:536])
	copy(serumVaultSigner[:], data[536:568])
	return
}

// readVaultBalances fetches current pool reserves for the monitor loop.
func (e *Executor) readVaultBalances(ctx context.Context, baseVault, quoteVault solana.PublicKey, source string) (reserveBase, reserveQuote uint64, err error) {
	if source == "pump_fun" {
		baseResult, err := e.rpc.GetTokenAccountBalance(ctx, baseVault, rpc.CommitmentConfirmed)
		if err != nil {
			return 0, 0, err
		}
		baseAmt, _ := strconv.ParseUint(baseResult.Value.Amount, 10, 64)
		quoteResult, err := e.rpc.GetBalance(ctx, quoteVault, rpc.CommitmentConfirmed)
		if err != nil {
			return 0, 0, err
		}
		return baseAmt, quoteResult.Value, nil
	}
	baseResult, err := e.rpc.GetTokenAccountBalance(ctx, baseVault, rpc.CommitmentConfirmed)
	if err != nil {
		return 0, 0, err
	}
	quoteResult, err := e.rpc.GetTokenAccountBalance(ctx, quoteVault, rpc.CommitmentConfirmed)
	if err != nil {
		return 0, 0, err
	}
	bAmt, _ := strconv.ParseUint(baseResult.Value.Amount, 10, 64)
	qAmt, _ := strconv.ParseUint(quoteResult.Value.Amount, 10, 64)
	return bAmt, qAmt, nil
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// getRPCAccountBytes fetches an account's raw data as bytes using base64 encoding.
// Works regardless of whether rpc.DataBytesOrJSON exposes GetBinaryData().
func getRPCAccountBytes(ctx context.Context, client *rpc.Client, pubkey solana.PublicKey) ([]byte, error) {
	info, err := client.GetAccountInfoWithOpts(ctx, pubkey, &rpc.GetAccountInfoOpts{
		Encoding:   solana.EncodingBase64,
		Commitment: rpc.CommitmentConfirmed,
	})
	if err != nil {
		return nil, err
	}
	if info == nil || info.Value == nil || info.Value.Data == nil {
		return nil, fmt.Errorf("account not found: %s", pubkey)
	}
	raw, err := json.Marshal(info.Value.Data)
	if err != nil {
		return nil, err
	}
	var parts []string
	if err := json.Unmarshal(raw, &parts); err != nil || len(parts) == 0 {
		return nil, fmt.Errorf("unexpected data format: %s", raw)
	}
	return base64.StdEncoding.DecodeString(parts[0])
}

// calcTipLamports scales a base tip up to 5× based on pool liquidity.
func calcTipLamports(liqSOL, baseTipSOL float64) uint64 {
	multiplier := math.Max(1.0, math.Min(5.0, liqSOL/10.0))
	return uint64(baseTipSOL * multiplier * 1e9)
}

// cpAmountOut applies constant-product formula with Raydium's 0.25% fee.
func cpAmountOut(amountIn, reserveIn, reserveOut uint64) uint64 {
	if reserveIn == 0 || reserveOut == 0 {
		return 0
	}
	inWithFee := amountIn * 9975
	num := inWithFee * reserveOut
	denom := reserveIn*10000 + inWithFee
	if denom == 0 {
		return 0
	}
	return num / denom
}

func safeDivide(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

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

// ─── rawInstruction ───────────────────────────────────────────────────────────

// rawInstruction implements solana.Instruction for programs without a Go SDK.
type rawInstruction struct {
	programID solana.PublicKey
	accounts  solana.AccountMetaSlice
	data      []byte
}

func (ix *rawInstruction) ProgramID() solana.PublicKey     { return ix.programID }
func (ix *rawInstruction) Accounts() []*solana.AccountMeta { return ix.accounts }
func (ix *rawInstruction) Data() ([]byte, error)           { return ix.data, nil }
