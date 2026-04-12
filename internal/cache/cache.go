package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Client wraps redis.Client with domain-specific helpers.
type Client struct {
	rdb *redis.Client
}

// New creates a Redis client. Call Ping to verify connectivity.
func New(addr, password string) *Client {
	rdb := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     password,
		DB:           0,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	})
	return &Client{rdb: rdb}
}

func (c *Client) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

func (c *Client) Close() error {
	return c.rdb.Close()
}

// ─── Token deduplication ──────────────────────────────────────────────────────

// IsSeenToken returns true if this token address was already processed.
func (c *Client) IsSeenToken(ctx context.Context, tokenAddr string) (bool, error) {
	n, err := c.rdb.Exists(ctx, seenKey(tokenAddr)).Result()
	return n > 0, err
}

// MarkTokenSeen records a token as processed; expires after 48 h to save memory.
func (c *Client) MarkTokenSeen(ctx context.Context, tokenAddr string) error {
	return c.rdb.Set(ctx, seenKey(tokenAddr), time.Now().Unix(), 48*time.Hour).Err()
}

// TryMarkTokenSeen atomically marks a token as seen using SET NX.
// Returns (true, nil) on the first call for this token — caller should proceed.
// Returns (false, nil) if the token was already seen — caller should skip.
// This eliminates the TOCTOU race between IsSeenToken + MarkTokenSeen.
func (c *Client) TryMarkTokenSeen(ctx context.Context, tokenAddr string) (bool, error) {
	return c.rdb.SetNX(ctx, seenKey(tokenAddr), time.Now().Unix(), 48*time.Hour).Result()
}

func seenKey(addr string) string { return "seen:token:" + addr }

// ─── Kill switch ──────────────────────────────────────────────────────────────

// KillSwitchActive returns true if the emergency stop is armed.
func (c *Client) KillSwitchActive(ctx context.Context) (bool, error) {
	val, err := c.rdb.Get(ctx, "killswitch").Result()
	if err == redis.Nil {
		return false, nil
	}
	return val == "1", err
}

// SetKillSwitch arms or disarms the kill switch.
func (c *Client) SetKillSwitch(ctx context.Context, active bool) error {
	v := "0"
	if active {
		v = "1"
	}
	return c.rdb.Set(ctx, "killswitch", v, 0).Err()
}

// ─── Positions ────────────────────────────────────────────────────────────────

// SavePosition persists a position. pos must be JSON-serialisable.
func (c *Client) SavePosition(ctx context.Context, tokenAddr string, pos any) error {
	data, err := json.Marshal(pos)
	if err != nil {
		return err
	}
	return c.rdb.HSet(ctx, "positions:open", tokenAddr, data).Err()
}

// DeletePosition removes a position from the open set.
func (c *Client) DeletePosition(ctx context.Context, tokenAddr string) error {
	return c.rdb.HDel(ctx, "positions:open", tokenAddr).Err()
}

// AllPositions returns all open positions as a map[tokenAddr]rawJSON.
func (c *Client) AllPositions(ctx context.Context) (map[string]string, error) {
	return c.rdb.HGetAll(ctx, "positions:open").Result()
}

// ─── Counters ────────────────────────────────────────────────────────────────

func (c *Client) Incr(ctx context.Context, name string) error {
	return c.rdb.Incr(ctx, "counter:"+name).Err()
}

func (c *Client) GetCounter(ctx context.Context, name string) (int64, error) {
	v, err := c.rdb.Get(ctx, "counter:"+name).Int64()
	if err == redis.Nil {
		return 0, nil
	}
	return v, err
}

// ─── P&L accumulator ─────────────────────────────────────────────────────────

// IncrPnL adds a signed SOL delta to the running total.
func (c *Client) IncrPnL(ctx context.Context, solDelta float64) error {
	return c.rdb.IncrByFloat(ctx, "pnl:sol", solDelta).Err()
}

// GetPnL returns the cumulative P&L in SOL.
func (c *Client) GetPnL(ctx context.Context) (float64, error) {
	v, err := c.rdb.Get(ctx, "pnl:sol").Float64()
	if err == redis.Nil {
		return 0, nil
	}
	return v, err
}

// StorePnLEntry appends a trade result to the P&L history list (capped at 500).
func (c *Client) StorePnLEntry(ctx context.Context, entry any) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	pipe := c.rdb.Pipeline()
	pipe.LPush(ctx, "pnl:history", data)
	pipe.LTrim(ctx, "pnl:history", 0, 499)
	_, err = pipe.Exec(ctx)
	return err
}

// GetPnLHistory returns the last N P&L entries as raw JSON strings.
func (c *Client) GetPnLHistory(ctx context.Context, limit int64) ([]string, error) {
	return c.rdb.LRange(ctx, "pnl:history", 0, limit-1).Result()
}

// ─── Deployer blacklist ───────────────────────────────────────────────────────

// BlacklistDeployer permanently blacklists an address (e.g. after a stop-loss rug).
func (c *Client) BlacklistDeployer(ctx context.Context, deployerAddr string) error {
	return c.rdb.SAdd(ctx, "blacklist:deployers", deployerAddr).Err()
}

// IsDeployerBlacklisted returns true if the address is in the permanent blacklist.
func (c *Client) IsDeployerBlacklisted(ctx context.Context, deployerAddr string) (bool, error) {
	return c.rdb.SIsMember(ctx, "blacklist:deployers", deployerAddr).Result()
}

// BlacklistedDeployers returns all blacklisted deployer addresses.
func (c *Client) BlacklistedDeployers(ctx context.Context) ([]string, error) {
	return c.rdb.SMembers(ctx, "blacklist:deployers").Result()
}

// ─── Leaderboard ─────────────────────────────────────────────────────────────

// LeaderboardEntry represents a single closed trade for ranking purposes.
type LeaderboardEntry struct {
	Token    string    `json:"token"`
	Symbol   string    `json:"symbol"`
	DeltaSOL float64   `json:"delta_sol"`
	Trigger  string    `json:"trigger"` // "take_profit" | "stop_loss" | "kill_switch"
	ClosedAt time.Time `json:"closed_at"`
}

// AddToLeaderboard records a closed trade in a Redis sorted set (score = deltaSOL).
// Keeps top 50 gainers and bottom 50 losers.
func (c *Client) AddToLeaderboard(ctx context.Context, entry LeaderboardEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	pipe := c.rdb.Pipeline()
	pipe.ZAdd(ctx, "leaderboard:trades", redis.Z{
		Score:  entry.DeltaSOL,
		Member: string(data),
	})
	pipe.ZRemRangeByRank(ctx, "leaderboard:trades", 50, -51)
	_, err = pipe.Exec(ctx)
	return err
}

// Leaderboard returns the top n gainers and top n losers.
func (c *Client) Leaderboard(ctx context.Context, n int) (gainers, losers []LeaderboardEntry, err error) {
	gRaw, err := c.rdb.ZRevRange(ctx, "leaderboard:trades", 0, int64(n-1)).Result()
	if err != nil && err != redis.Nil {
		return nil, nil, fmt.Errorf("leaderboard gainers: %w", err)
	}
	for _, raw := range gRaw {
		var e LeaderboardEntry
		if json.Unmarshal([]byte(raw), &e) == nil {
			gainers = append(gainers, e)
		}
	}

	lRaw, err := c.rdb.ZRange(ctx, "leaderboard:trades", 0, int64(n-1)).Result()
	if err != nil && err != redis.Nil {
		return nil, nil, fmt.Errorf("leaderboard losers: %w", err)
	}
	for _, raw := range lRaw {
		var e LeaderboardEntry
		if json.Unmarshal([]byte(raw), &e) == nil {
			losers = append(losers, e)
		}
	}
	return gainers, losers, nil
}

// ─── Trajectory ───────────────────────────────────────────────────────────────

// TrajectorySnap is a single timestamped observation of an open position's ROI.
type TrajectorySnap struct {
	Label      string    `json:"label"`       // "30s", "1m", "2m", "5m", "10m"
	ElapsedS   int64     `json:"elapsed_s"`
	ROIPct     float64   `json:"roi_pct"`
	CurrentSOL float64   `json:"current_sol"`
	RecordedAt time.Time `json:"recorded_at"`
}

// AppendTrajectorySnap records one snapshot for a token. The list expires after 7 days.
func (c *Client) AppendTrajectorySnap(ctx context.Context, tokenAddr string, snap TrajectorySnap) error {
	data, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	key := "trajectory:" + tokenAddr
	pipe := c.rdb.Pipeline()
	pipe.RPush(ctx, key, data)
	pipe.Expire(ctx, key, 7*24*time.Hour)
	_, err = pipe.Exec(ctx)
	return err
}

// GetTrajectory returns all recorded trajectory snapshots for a token (oldest first).
func (c *Client) GetTrajectory(ctx context.Context, tokenAddr string) ([]TrajectorySnap, error) {
	raw, err := c.rdb.LRange(ctx, "trajectory:"+tokenAddr, 0, -1).Result()
	if err != nil && err != redis.Nil {
		return nil, err
	}
	snaps := make([]TrajectorySnap, 0, len(raw))
	for _, r := range raw {
		var s TrajectorySnap
		if json.Unmarshal([]byte(r), &s) == nil {
			snaps = append(snaps, s)
		}
	}
	return snaps, nil
}

// ─── Generic helpers ──────────────────────────────────────────────────────────

// SetString is a generic key-value setter.
func (c *Client) SetString(ctx context.Context, key, value string, ttl time.Duration) error {
	return c.rdb.Set(ctx, key, value, ttl).Err()
}

// GetString retrieves a string value; returns ("", nil) if key is absent.
func (c *Client) GetString(ctx context.Context, key string) (string, error) {
	v, err := c.rdb.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", nil
	}
	return v, err
}

// Publish sends a message on a Redis pub/sub channel.
func (c *Client) Publish(ctx context.Context, channel, message string) error {
	return c.rdb.Publish(ctx, channel, message).Err()
}

// StatsSnapshot is returned by Stats.
type StatsSnapshot struct {
	Scanned          int64   `json:"tokens_scanned"`
	Filtered         int64   `json:"tokens_filtered"`
	SnipesAttempted  int64   `json:"snipes_attempted"`
	SnipesSuccessful int64   `json:"snipes_successful"`
	PnLSOL           float64 `json:"pnl_sol"`
	KillSwitch       bool    `json:"kill_switch"`
}

// Stats aggregates counters into a single snapshot.
func (c *Client) Stats(ctx context.Context) (*StatsSnapshot, error) {
	pipe := c.rdb.Pipeline()
	scanned := pipe.Get(ctx, "counter:scanned")
	filtered := pipe.Get(ctx, "counter:filtered")
	attempted := pipe.Get(ctx, "counter:snipes_attempted")
	successful := pipe.Get(ctx, "counter:snipes_successful")
	pnl := pipe.Get(ctx, "pnl:sol")
	ks := pipe.Get(ctx, "killswitch")
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, fmt.Errorf("stats pipeline: %w", err)
	}

	snap := &StatsSnapshot{}
	snap.Scanned, _ = scanned.Int64()
	snap.Filtered, _ = filtered.Int64()
	snap.SnipesAttempted, _ = attempted.Int64()
	snap.SnipesSuccessful, _ = successful.Int64()
	snap.PnLSOL, _ = pnl.Float64()
	snap.KillSwitch = ks.Val() == "1"
	return snap, nil
}
