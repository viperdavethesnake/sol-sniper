// Package dataset writes structured JSONL observation records for every token
// the bot encounters — both filtered rejections and closed positions.
// Each line is a self-contained JSON object compatible with Pandas / TensorFlow.
package dataset

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// DefaultPath is where the bot writes its training data inside the container.
const DefaultPath = "/data/sol_sniper_training_v1.jsonl"

// Metadata holds static properties measured at token detection time.
type Metadata struct {
	DEX            string  `json:"dex"`              // "Raydium_V4" | "Pump_Fun"
	InitLiqSOL     float64 `json:"init_liq_sol"`     // SOL in quote vault at detection
	TaxPct         float64 `json:"tax_pct"`          // simulated round-trip loss %
	IsRenounced    bool    `json:"is_renounced"`     // true if mintAuthority == nil
	Symbol         string  `json:"symbol"`
	IsPumpFun      bool    `json:"is_pump_fun"`      // true for Pump.fun bonding curve tokens
	PriorityTipSOL float64 `json:"priority_tip_sol"` // Jito tip paid in SOL (0 if not executed)
	BundleLanded   bool    `json:"bundle_landed"`    // whether the Jito bundle was confirmed on-chain

	// Fields added for ML feature richness.
	MintDecimals       uint8   `json:"mint_decimals"`                  // SPL token decimal places
	TopHoldersPct      float64 `json:"top_holders_pct"`                // % supply held by top-2 accounts
	HoldersCount       int     `json:"holders_count"`                  // len(GetTokenLargestAccounts result)
	SlotAtDetection    uint64  `json:"slot_at_detection"`              // Solana slot when event was received
	DetectionLatencyMs int64   `json:"detection_latency_ms"`           // ms from listener emit to analyzer decision
	SolToGraduation    float64 `json:"sol_to_graduation,omitempty"`    // Pump.fun only: SOL needed to graduate (85 − real_sol)
}

// Execution holds the simulated entry quote computed before opening a position.
type Execution struct {
	EntryTokensRaw  string  `json:"entry_tokens_raw"`   // raw token units for MaxBuySOL
	EntryPriceSOL   float64 `json:"entry_price_sol"`    // SOL per token
	PriceImpactPct  float64 `json:"price_impact_pct"`   // % the buy moves the pool price
	FrictionScore   float64 `json:"friction_score"`     // simulated round-trip loss %
}

// TrajectoryPoint is one ROI observation from the position monitor.
type TrajectoryPoint struct {
	TPlus int     `json:"t_plus"` // seconds since entry
	ROI   float64 `json:"roi"`    // current_sol / entry_sol (1.0 = break-even)
}

// Record is a complete observation written to the JSONL file.
// All fields are present for every record; Execution is nil for filtered tokens.
type Record struct {
	ScannedAt    time.Time         `json:"scanned_at"`
	TokenAddress string            `json:"token_address"` // base58 mint address
	Metadata     Metadata          `json:"metadata"`
	Execution    *Execution        `json:"execution,omitempty"`
	Trajectory   []TrajectoryPoint `json:"trajectory"`
	FinalLabel   string            `json:"final_label"` // "TAKE_PROFIT"|"TIMEOUT"|"STOP_LOSS"|"REJECTED_*"
}

// Writer appends JSON Lines records to a single file. Safe for concurrent use.
type Writer struct {
	mu sync.Mutex
	f  *os.File
}

// New opens (or creates) the JSONL file for appending.
// The parent directory is created if it does not exist.
func New(path string) (*Writer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	log.Info().Str("path", path).Msg("Dataset writer opened")
	return &Writer{f: f}, nil
}

// Write appends one record as a newline-terminated JSON object.
func (w *Writer) Write(rec Record) {
	data, err := json.Marshal(rec)
	if err != nil {
		log.Warn().Err(err).Str("token", rec.TokenAddress).Msg("dataset: marshal failed")
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _ = w.f.Write(data)
	_, _ = w.f.Write([]byte{'\n'})
}

// Close flushes and closes the underlying file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}
