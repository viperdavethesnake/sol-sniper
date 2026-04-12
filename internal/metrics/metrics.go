// Package metrics defines all Prometheus metrics for the bot.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// TokensScanned counts every new token event received from program logs.
	TokensScanned = promauto.NewCounter(prometheus.CounterOpts{
		Name: "sniper_tokens_scanned_total",
		Help: "Total new token events received from Raydium/Pump.fun program logs.",
	})

	// TokensFiltered counts tokens rejected before execution, labeled by reason.
	TokensFiltered = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "sniper_tokens_filtered_total",
		Help: "Tokens rejected before execution, labeled by filter reason.",
	}, []string{"reason"})

	// SnipesAttempted counts Jito bundles submitted (not necessarily landed).
	SnipesAttempted = promauto.NewCounter(prometheus.CounterOpts{
		Name: "sniper_snipes_attempted_total",
		Help: "Jito bundles submitted to the block engine.",
	})

	// SnipesSuccessful counts bundles that landed on-chain.
	SnipesSuccessful = promauto.NewCounter(prometheus.CounterOpts{
		Name: "sniper_snipes_successful_total",
		Help: "Buy transactions confirmed on-chain.",
	})

	// SellsExecuted counts sell bundles submitted, labeled by trigger.
	SellsExecuted = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "sniper_sells_executed_total",
		Help: "Sell transactions submitted, labeled by trigger (take_profit, stop_loss, kill_switch).",
	}, []string{"trigger"})

	// AnalysisDuration tracks time spent analyzing a single token.
	AnalysisDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "sniper_analysis_duration_seconds",
		Help:    "Time to analyze a token (freeze/mint check + holder check + CP sim).",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5},
	})

	// ExecutionDuration tracks time from decision to Jito bundle submission.
	ExecutionDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "sniper_execution_duration_seconds",
		Help:    "Time from snipe decision to Jito bundle submission.",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2},
	})

	// RPCLatency tracks individual RPC call latency by method.
	RPCLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "sniper_rpc_latency_seconds",
		Help:    "RPC call latency by method.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
	}, []string{"method"})

	// ActivePositions is the current count of open positions.
	ActivePositions = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "sniper_active_positions",
		Help: "Number of currently open positions.",
	})

	// PnLSOL is the cumulative profit/loss across all closed positions, in SOL.
	PnLSOL = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "sniper_pnl_sol",
		Help: "Cumulative realised P&L in SOL (negative means loss).",
	})

	// JitoTipSOL tracks the Jito tip used in the last bundle submission, in SOL.
	JitoTipSOL = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "sniper_jito_tip_sol",
		Help: "Jito tip paid in the last bundle submission, in SOL.",
	})

	// JitoTipSuccessRatio tracks the rolling ratio of tip amounts to landed bundles.
	// Updated by the executor on each bundle result.
	JitoTipSuccessRatio = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "sniper_jito_tip_success_ratio",
		Help: "Rolling ratio of Jito tips paid to bundles that landed (for ML tip optimisation).",
	})

	// WSReconnections counts WebSocket reconnection events.
	WSReconnections = promauto.NewCounter(prometheus.CounterOpts{
		Name: "sniper_ws_reconnections_total",
		Help: "Number of WebSocket reconnection events.",
	})

	// FrictionScore tracks the last observed friction score (round-trip loss %).
	FrictionScore = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "sniper_friction_score_last",
		Help: "Friction score of the last analyzed token (simulated round-trip loss %).",
	})

	// EntryPriceImpact tracks the price impact of the last simulated entry quote.
	EntryPriceImpact = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "sniper_entry_price_impact_pct",
		Help: "Price impact of the last simulated buy entry as a percentage.",
	})

	// SignalsDropped counts TradeSignals discarded because tradeCh was full.
	SignalsDropped = promauto.NewCounter(prometheus.CounterOpts{
		Name: "sniper_signals_dropped_total",
		Help: "TradeSignals dropped due to a full tradeCh buffer (executor too slow).",
	})

	// AnalyzerPanics counts recovered panics inside the per-token process() goroutine.
	AnalyzerPanics = promauto.NewCounter(prometheus.CounterOpts{
		Name: "sniper_analyzer_panics_total",
		Help: "Panics recovered inside analyzer.process() — indicates malformed on-chain data.",
	})

	// TokensScannedBySource counts new token events labeled by source DEX.
	TokensScannedBySource = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "sniper_tokens_scanned_by_source_total",
		Help: "New token events received from program logs, labeled by source DEX (raydium, pump_fun).",
	}, []string{"source"})

	// PositionsOpenedBySource counts buy positions opened, labeled by source DEX.
	PositionsOpenedBySource = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "sniper_positions_opened_by_source_total",
		Help: "Buy positions opened (SIM or live), labeled by source DEX (raydium, pump_fun).",
	}, []string{"source"})
)
