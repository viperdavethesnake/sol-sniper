package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Config holds all runtime configuration loaded from environment variables.
type Config struct {
	// RPC
	RPCURL      string // Helius/Triton HTTPS endpoint
	WSEndpoint  string // Helius/Triton WSS endpoint (for logsSubscribe)
	JitoURL     string // Jito block engine URL (no scheme, no port — TLS port 443 assumed)

	// Wallet
	PrivateKey string // base58-encoded keypair
	WalletAddr string // base58-encoded public key

	// Redis
	RedisAddr     string
	RedisPassword string

	// Trading parameters
	MinLiqSOL       float64 // minimum pool SOL liquidity to consider
	MaxBuySOL       float64 // maximum SOL to spend per snipe
	TakeProfitPct   float64 // e.g. 100 = sell when up 100%
	StopLossPct     float64 // e.g. 30  = sell when down 30%
	SlippagePct     float64 // buy/sell slippage tolerance
	MonitorInterval int     // seconds between position monitor ticks
	MaxFrictionPct  float64 // skip token if round-trip loss % exceeds this; 0 = no limit

	// Solana program IDs
	RaydiumAMMProgram string // Raydium AMM V4 program ID
	PumpFunProgram    string // Pump.fun program ID

	// API
	APIPort string

	// Simulation
	SimulationMode bool // analyse and log but never submit real transactions
}

// Load reads configuration from environment (and optional .env file).
func Load() (*Config, error) {
	_ = godotenv.Load() // best-effort; no error if .env is absent

	simMode := getEnv("SIMULATION_MODE", "false") == "true"

	privKeyDefault := ""
	walletDefault := ""
	if simMode {
		// Dummy keypair so executor can be constructed without real credentials.
		privKeyDefault = "11111111111111111111111111111111"
		walletDefault = "11111111111111111111111111111111"
	}

	cfg := &Config{
		RPCURL:         mustEnv("RPC_URL"),
		WSEndpoint:     mustEnv("RPC_WS_ENDPOINT"),
		JitoURL:        getEnv("JITO_BLOCK_ENGINE_URL", "frankfurt.mainnet.block-engine.jito.wtf"),
		SimulationMode: simMode,
		PrivateKey:     getEnvOr("BOT_PRIVATE_KEY", privKeyDefault),
		WalletAddr:     getEnvOr("BOT_WALLET_ADDR", walletDefault),
		RedisAddr:      getEnv("REDIS_ADDR", "redis:6379"),
		RedisPassword:  getEnv("REDIS_PASSWORD", ""),
		APIPort:        getEnv("API_PORT", "8080"),
		RaydiumAMMProgram: getEnv("RAYDIUM_AMM_PROGRAM", "675kPX9MHTjS2zt1qfr1NYHuzeLXfQM9H24wFSUt1Mp8"),
		PumpFunProgram:    getEnv("PUMP_FUN_PROGRAM", "6EF8rrecthR5Dkzon8Nwu78hRvfCKubJ14M5uBEwF6P"),
	}

	cfg.MinLiqSOL = mustFloat(getEnv("MIN_LIQUIDITY_SOL", "5.0"))
	cfg.MaxBuySOL = mustFloat(getEnv("MAX_BUY_SOL", "0.1"))
	cfg.TakeProfitPct = mustFloat(getEnv("TAKE_PROFIT_PCT", "100"))
	cfg.StopLossPct = mustFloat(getEnv("STOP_LOSS_PCT", "30"))
	cfg.SlippagePct = mustFloat(getEnv("SLIPPAGE_PCT", "15"))
	cfg.MaxFrictionPct = mustFloat(getEnv("MAX_FRICTION_PCT", "20.0"))

	interval, _ := strconv.Atoi(getEnv("POSITION_MONITOR_INTERVAL", "30"))
	cfg.MonitorInterval = interval

	if !simMode {
		if cfg.PrivateKey == "" {
			return nil, fmt.Errorf("BOT_PRIVATE_KEY is required when SIMULATION_MODE is not true")
		}
		if cfg.WalletAddr == "" {
			return nil, fmt.Errorf("BOT_WALLET_ADDR is required when SIMULATION_MODE is not true")
		}
	}

	return cfg, nil
}

// MaxBuyLamports converts MaxBuySOL to lamports (1 SOL = 1e9 lamports).
func (c *Config) MaxBuyLamports() uint64 {
	return uint64(c.MaxBuySOL * 1e9)
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		panic(fmt.Sprintf("required environment variable %q is not set", key))
	}
	return v
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func mustFloat(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return f
}
