// Package listener maintains persistent WebSocket subscriptions to the Raydium AMM V4
// and Pump.fun programs on Solana and emits TokenEvent for every new pool or token launch.
package listener

import (
	"context"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/ws"
	"github.com/rs/zerolog/log"

	"github.com/sol-sniper/bot/internal/config"
	"github.com/sol-sniper/bot/internal/metrics"
)

// TokenEvent carries the data decoded from a new pool or token-launch program log.
type TokenEvent struct {
	Mint       solana.PublicKey // new token mint address
	Pool       solana.PublicKey // Raydium AMM ID or Pump.fun bonding curve address
	Source     string           // "raydium" | "pump_fun"
	LiqSOL     float64          // initial SOL balance of the quote vault / bonding curve
	Signature  string           // transaction signature that triggered the event
	Slot       uint64
	DetectedAt time.Time
}

// Listener subscribes to Raydium and Pump.fun program logs and forwards
// decoded TokenEvents to eventCh.
type Listener struct {
	cfg     *config.Config
	rpc     *rpc.Client
	eventCh chan<- *TokenEvent

	raydiumProg solana.PublicKey
	pumpFunProg solana.PublicKey
}

// New creates a Listener. rpcClient is used for follow-up account queries.
func New(cfg *config.Config, eventCh chan<- *TokenEvent) (*Listener, error) {
	return &Listener{
		cfg:         cfg,
		rpc:         rpc.New(cfg.RPCURL),
		eventCh:     eventCh,
		raydiumProg: solana.MustPublicKeyFromBase58(cfg.RaydiumAMMProgram),
		pumpFunProg: solana.MustPublicKeyFromBase58(cfg.PumpFunProgram),
	}, nil
}

// Run opens WebSocket subscriptions and blocks until ctx is cancelled.
// On WebSocket error it waits 5 s then reconnects (identical to base-sniper pattern).
func (l *Listener) Run(ctx context.Context) {
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		if err := l.subscribe(ctx); err != nil {
			log.Error().Err(err).Msg("listener: WebSocket error — reconnecting in 5s")
			metrics.WSReconnections.Inc()
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
}

// subscribe opens the WS connection, subscribes to both programs, and fans out
// goroutines to handle incoming notifications. Returns on error or ctx cancel.
func (l *Listener) subscribe(ctx context.Context) error {
	wsClient, err := ws.Connect(ctx, l.cfg.WSEndpoint)
	if err != nil {
		return err
	}
	defer wsClient.Close()

	raydiumSub, err := wsClient.LogsSubscribeMentions(l.raydiumProg, rpc.CommitmentConfirmed)
	if err != nil {
		return err
	}
	defer raydiumSub.Unsubscribe()

	pumpSub, err := wsClient.LogsSubscribeMentions(l.pumpFunProg, rpc.CommitmentConfirmed)
	if err != nil {
		return err
	}
	defer pumpSub.Unsubscribe()

	log.Info().
		Str("raydium", l.cfg.RaydiumAMMProgram).
		Str("pump_fun", l.cfg.PumpFunProgram).
		Msg("listener: WebSocket subscriptions active")

	raydiumCh := make(chan *ws.LogResult, 64)
	pumpCh := make(chan *ws.LogResult, 64)

	// Receiver goroutines — forward raw results to buffered channels.
	go func() {
		for {
			result, err := raydiumSub.Recv()
			if err != nil {
				close(raydiumCh)
				return
			}
			raydiumCh <- result
		}
	}()
	go func() {
		for {
			result, err := pumpSub.Recv()
			if err != nil {
				close(pumpCh)
				return
			}
			pumpCh <- result
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case result, ok := <-raydiumCh:
			if !ok {
				return nil
			}
			go l.handleRaydium(ctx, result)
		case result, ok := <-pumpCh:
			if !ok {
				return nil
			}
			go l.handlePumpFun(ctx, result)
		}
	}
}

// handleRaydium processes a log notification from the Raydium AMM program.
// We filter for transactions that contain "initialize2" in the logs, which
// signals a new AMM pool being created.
func (l *Listener) handleRaydium(ctx context.Context, result *ws.LogResult) {
	if result.Value.Err != nil {
		return // failed transaction — skip
	}

	isInit := false
	for _, line := range result.Value.Logs {
		if strings.Contains(line, "initialize2") {
			isInit = true
			break
		}
	}
	if !isInit {
		return
	}

	sig := result.Value.Signature.String()
	log.Debug().Str("sig", sig).Msg("listener: Raydium initialize2 detected")

	// Fetch the transaction to extract pool accounts from instruction data.
	maxVer := uint64(0)
	tx, err := l.rpc.GetTransaction(
		ctx,
		result.Value.Signature,
		&rpc.GetTransactionOpts{
			Encoding:                       solana.EncodingBase64,
			Commitment:                     rpc.CommitmentConfirmed,
			MaxSupportedTransactionVersion: &maxVer,
		},
	)
	if err != nil || tx == nil || tx.Transaction == nil {
		log.Warn().Err(err).Str("sig", sig).Msg("listener: failed to fetch Raydium tx")
		return
	}

	decoded, err := tx.Transaction.GetTransaction()
	if err != nil {
		log.Warn().Err(err).Str("sig", sig).Msg("listener: failed to decode Raydium tx")
		return
	}

	// Raydium AMM V4 initialize2 instruction account layout (per-instruction indices):
	//   ix.Accounts[4]  → amm     (pool state account — pool ID)
	//   ix.Accounts[8]  → coin_mint (base token)
	//   ix.Accounts[9]  → pc_mint  (quote token — must be WSOL)
	//   ix.Accounts[11] → pc_vault (WSOL quote vault)
	//
	// NOTE: msg.AccountKeys is a deduplicated flat list across ALL instructions.
	// We must find the matching instruction and use ix.Accounts[N] as indices into
	// msg.AccountKeys — NOT positional offsets into accts directly.
	msg := decoded.Message
	accts := msg.AccountKeys

	wsolPubkey := solana.MustPublicKeyFromBase58("So11111111111111111111111111111111111111112")

	var ammID, coinMint, pcVault solana.PublicKey
	found := false
	for _, ix := range msg.Instructions {
		if int(ix.ProgramIDIndex) >= len(accts) || accts[ix.ProgramIDIndex] != l.raydiumProg {
			continue
		}
		if len(ix.Accounts) < 12 {
			continue
		}
		// Guard: account index values must be within the flat AccountKeys slice.
		if int(ix.Accounts[4]) >= len(accts) || int(ix.Accounts[8]) >= len(accts) ||
			int(ix.Accounts[9]) >= len(accts) || int(ix.Accounts[11]) >= len(accts) {
			continue
		}
		pcMint := accts[ix.Accounts[9]]
		if pcMint != wsolPubkey {
			// Not a SOL-paired pool — skip.
			return
		}
		ammID = accts[ix.Accounts[4]]
		coinMint = accts[ix.Accounts[8]]
		pcVault = accts[ix.Accounts[11]]
		found = true
		break
	}
	if !found {
		return
	}

	// Query the quote vault balance to compute initial liquidity in SOL.
	liqSOL, err := l.getTokenAccountSOL(ctx, pcVault)
	if err != nil {
		log.Warn().Err(err).Str("vault", pcVault.String()).Msg("listener: failed to read quote vault")
		return
	}

	ev := &TokenEvent{
		Mint:       coinMint,
		Pool:       ammID,
		Source:     "raydium",
		LiqSOL:     liqSOL,
		Signature:  sig,
		Slot:       tx.Slot,
		DetectedAt: time.Now(),
	}

	metrics.TokensScanned.Inc()
	metrics.TokensScannedBySource.WithLabelValues("raydium").Inc()
	select {
	case l.eventCh <- ev:
	default:
		log.Warn().Str("mint", coinMint.String()).Msg("listener: eventCh full — dropping Raydium event")
	}
}

// handlePumpFun processes a log notification from the Pump.fun program.
// We filter for transactions that contain "Instruction: Create" in the logs.
func (l *Listener) handlePumpFun(ctx context.Context, result *ws.LogResult) {
	if result.Value.Err != nil {
		return
	}

	isCreate := false
	for _, line := range result.Value.Logs {
		if strings.Contains(line, "Instruction: Create") {
			isCreate = true
			break
		}
	}
	if !isCreate {
		return
	}

	sig := result.Value.Signature.String()
	log.Debug().Str("sig", sig).Msg("listener: Pump.fun create detected")

	maxVer := uint64(0)
	tx, err := l.rpc.GetTransaction(
		ctx,
		result.Value.Signature,
		&rpc.GetTransactionOpts{
			Encoding:                       solana.EncodingBase64,
			Commitment:                     rpc.CommitmentConfirmed,
			MaxSupportedTransactionVersion: &maxVer,
		},
	)
	if err != nil || tx == nil || tx.Transaction == nil {
		log.Warn().Err(err).Str("sig", sig).Msg("listener: failed to fetch Pump.fun tx")
		return
	}

	decoded, err := tx.Transaction.GetTransaction()
	if err != nil {
		log.Warn().Err(err).Str("sig", sig).Msg("listener: failed to decode Pump.fun tx")
		return
	}

	// Pump.fun create instruction account layout (per-instruction indices):
	//   ix.Accounts[0] → mint          (new token mint)
	//   ix.Accounts[2] → bonding_curve (PDA — holds SOL)
	//
	// NOTE: must use ix.Accounts[N] as indices into msg.AccountKeys, not direct offsets.
	msg := decoded.Message
	accts := msg.AccountKeys

	var mint, bondingCurve solana.PublicKey
	found := false
	for _, ix := range msg.Instructions {
		if int(ix.ProgramIDIndex) >= len(accts) || accts[ix.ProgramIDIndex] != l.pumpFunProg {
			continue
		}
		if len(ix.Accounts) < 3 {
			continue
		}
		// Guard: account index values must be within the flat AccountKeys slice.
		if int(ix.Accounts[0]) >= len(accts) || int(ix.Accounts[2]) >= len(accts) {
			continue
		}
		mint = accts[ix.Accounts[0]]
		bondingCurve = accts[ix.Accounts[2]]
		found = true
		break
	}
	if !found {
		return
	}

	// Get SOL balance of bonding curve account.
	balResp, err := l.rpc.GetBalance(ctx, bondingCurve, rpc.CommitmentConfirmed)
	if err != nil {
		log.Warn().Err(err).Str("curve", bondingCurve.String()).Msg("listener: failed to read bonding curve balance")
		return
	}
	liqSOL := float64(balResp.Value) / 1e9

	ev := &TokenEvent{
		Mint:       mint,
		Pool:       bondingCurve,
		Source:     "pump_fun",
		LiqSOL:     liqSOL,
		Signature:  sig,
		Slot:       tx.Slot,
		DetectedAt: time.Now(),
	}

	metrics.TokensScanned.Inc()
	metrics.TokensScannedBySource.WithLabelValues("pump_fun").Inc()
	select {
	case l.eventCh <- ev:
	default:
		log.Warn().Str("mint", mint.String()).Msg("listener: eventCh full — dropping Pump.fun event")
	}
}

// getTokenAccountSOL returns the SOL-equivalent balance of a WSOL token account
// (1 WSOL = 1 SOL, 9 decimals).
func (l *Listener) getTokenAccountSOL(ctx context.Context, account solana.PublicKey) (float64, error) {
	result, err := l.rpc.GetTokenAccountBalance(ctx, account, rpc.CommitmentConfirmed)
	if err != nil {
		return 0, err
	}
	if result == nil || result.Value == nil {
		return 0, nil
	}
	if result.Value.UiAmount == nil {
		return 0, nil
	}
	return *result.Value.UiAmount, nil
}
