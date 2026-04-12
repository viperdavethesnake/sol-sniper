// Package api provides a REST API for the dashboard and operators.
//
// Endpoints:
//
//	GET  /health              — liveness probe
//	GET  /metrics             — Prometheus metrics (scraped by Prometheus)
//	GET  /api/stats           — counters, P&L, kill-switch status
//	GET  /api/positions       — all open positions
//	GET  /api/leaderboard     — top gainers and losers
//	GET  /api/trajectory/:token — trajectory snapshots for a token
//	POST /api/killswitch      — arm or disarm the kill switch
package api

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog/log"

	"github.com/sol-sniper/bot/internal/cache"
	"github.com/sol-sniper/bot/internal/config"
	"github.com/sol-sniper/bot/internal/executor"
)

// Server is the REST API HTTP server.
type Server struct {
	cfg    *config.Config
	cache  *cache.Client
	exec   *executor.Executor
	engine *gin.Engine
}

// New creates and configures the API server.
func New(cfg *config.Config, c *cache.Client, exec *executor.Executor) *Server {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(requestLogger())

	s := &Server{cfg: cfg, cache: c, exec: exec, engine: r}
	s.registerRoutes()
	return s
}

// Run starts the HTTP server; blocks until ctx is done.
func (s *Server) Run(ctx context.Context) {
	srv := &http.Server{
		Addr:    ":" + s.cfg.APIPort,
		Handler: s.engine,
	}

	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()

	log.Info().Str("port", s.cfg.APIPort).Msg("API server listening")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error().Err(err).Msg("API server error")
	}
}

func (s *Server) registerRoutes() {
	s.engine.GET("/health", s.handleHealth)
	s.engine.GET("/api/health", s.handleHealth)
	s.engine.GET("/metrics", gin.WrapH(promhttp.Handler()))

	api := s.engine.Group("/api")
	api.GET("/stats", s.handleStats)
	api.GET("/positions", s.handlePositions)
	api.GET("/leaderboard", s.handleLeaderboard)
	api.GET("/trajectory/:token", s.handleTrajectory)
	api.POST("/killswitch", s.handleKillSwitch)
}

// ─── Handlers ────────────────────────────────────────────────────────────────

func (s *Server) handleHealth(c *gin.Context) {
	ctx := c.Request.Context()
	redisOK := s.cache.Ping(ctx) == nil
	c.JSON(http.StatusOK, gin.H{
		"status":          "ok",
		"redis_ok":        redisOK,
		"simulation_mode": s.cfg.SimulationMode,
		"rpc_endpoint":    s.cfg.RPCURL,
	})
}

func (s *Server) handleStats(c *gin.Context) {
	ctx := c.Request.Context()
	snap, err := s.cache.Stats(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	active := s.exec.ActivePositions()
	snap.Scanned, _ = s.cache.GetCounter(ctx, "scanned")
	snap.Filtered, _ = s.cache.GetCounter(ctx, "filtered")
	snap.SnipesAttempted, _ = s.cache.GetCounter(ctx, "snipes_attempted")
	snap.SnipesSuccessful, _ = s.cache.GetCounter(ctx, "snipes_successful")
	snap.PnLSOL, _ = s.cache.GetPnL(ctx)
	snap.KillSwitch, _ = s.cache.KillSwitchActive(ctx)

	c.JSON(http.StatusOK, gin.H{
		"tokens_scanned":    snap.Scanned,
		"tokens_filtered":   snap.Filtered,
		"snipes_attempted":  snap.SnipesAttempted,
		"snipes_successful": snap.SnipesSuccessful,
		"active_positions":  len(active),
		"pnl_sol":           snap.PnLSOL,
		"kill_switch":       snap.KillSwitch,
	})
}

func (s *Server) handlePositions(c *gin.Context) {
	positions := s.exec.ActivePositions()
	c.JSON(http.StatusOK, gin.H{"positions": positions, "count": len(positions)})
}

func (s *Server) handleLeaderboard(c *gin.Context) {
	gainers, losers, err := s.cache.Leaderboard(c.Request.Context(), 5)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if gainers == nil {
		gainers = []cache.LeaderboardEntry{}
	}
	if losers == nil {
		losers = []cache.LeaderboardEntry{}
	}
	c.JSON(http.StatusOK, gin.H{
		"top_gainers": gainers,
		"top_losers":  losers,
	})
}

func (s *Server) handleTrajectory(c *gin.Context) {
	token := c.Param("token")
	snaps, err := s.cache.GetTrajectory(c.Request.Context(), token)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": token, "trajectory": snaps, "count": len(snaps)})
}

// handleKillSwitch arms or disarms the kill switch.
//
// Body: {"active": true}  → arm (sell all positions).
// Body: {"active": false} → disarm (re-enable buying).
func (s *Server) handleKillSwitch(c *gin.Context) {
	var body struct {
		Active bool `json:"active"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": `body must be {"active": bool}`})
		return
	}

	var err error
	if body.Active {
		err = s.exec.KillSwitch(c.Request.Context())
	} else {
		err = s.exec.DisarmKillSwitch(c.Request.Context())
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	state := "disarmed"
	if body.Active {
		state = "armed — all positions being sold"
	}
	c.JSON(http.StatusOK, gin.H{"kill_switch": state})
}

// ─── Middleware ────────────────────────────────────────────────────────────────

func requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		log.Debug().
			Int("status", c.Writer.Status()).
			Str("method", c.Request.Method).
			Str("path", c.Request.URL.Path).
			Msg("API request")
	}
}
