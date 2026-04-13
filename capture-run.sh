#!/usr/bin/env bash
# capture-run.sh — snapshot a running sol-sniper run into a timestamped archive.
#
# Usage:
#   ./capture-run.sh [RUN_TAG]
#
# RUN_TAG defaults to run<NNN>-<YYYYMMDD>-<HHMM> where NNN is auto-incremented
# from the highest existing run directory.
#
# The script is safe to run while the stack is live — it never stops containers.
# Run it once to "save state", or after stopping to create the final archive.
#
# Output: ./<RUN_TAG>/ directory + ./<RUN_TAG>.tar.gz
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# ── Determine run tag ────────────────────────────────────────────────────────
if [[ -n "${1:-}" ]]; then
  TAG="$1"
else
  # Auto-increment: find highest runNNN prefix
  LAST_NUM=$(ls -d run[0-9][0-9][0-9]-* 2>/dev/null | sed 's/run\([0-9]*\)-.*/\1/' | sort -n | tail -1 || true)
  NEXT_NUM=$(printf "%03d" $(( ${LAST_NUM:-0} + 1 )))
  TAG="run${NEXT_NUM}-$(date +%Y%m%d-%H%M)"
fi

OUT_DIR="$SCRIPT_DIR/$TAG"
echo "[capture] Writing to $OUT_DIR"
mkdir -p "$OUT_DIR"

# ── Helper: run docker compose exec without failing the whole script ─────────
dc_exec() {
  docker compose exec -T "$@" 2>/dev/null || true
}

# ── 1. Environment snapshot (redact private key) ─────────────────────────────
echo "[capture] Snapshotting .env (redacted)..."
sed 's/\(BOT_PRIVATE_KEY\s*=\s*\).*/\1<REDACTED>/' .env > "$OUT_DIR/env-snapshot.txt"
echo "CAPTURE_TIME=$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$OUT_DIR/env-snapshot.txt"

# ── 2. Git state ─────────────────────────────────────────────────────────────
echo "[capture] Snapshotting git state..."
git log --oneline -20 > "$OUT_DIR/git-log.txt" 2>/dev/null || true
git diff HEAD > "$OUT_DIR/git-diff.txt" 2>/dev/null || true
git status --short > "$OUT_DIR/git-status.txt" 2>/dev/null || true

# ── 3. Container logs ─────────────────────────────────────────────────────────
echo "[capture] Collecting container logs..."
docker compose logs --no-color sol-sniper-bot > "$OUT_DIR/bot.log" 2>/dev/null || \
  docker logs sol-sniper-bot --no-log-prefix > "$OUT_DIR/bot.log" 2>/dev/null || true

# ── 4. Redis state ───────────────────────────────────────────────────────────
echo "[capture] Dumping Redis state..."
{
  echo "=== counter:snipes_attempted ==="
  dc_exec redis redis-cli GET "counter:snipes_attempted"
  echo ""
  echo "=== pnl:sol ==="
  dc_exec redis redis-cli GET "pnl:sol"
  echo ""
  echo "=== positions:open (all fields) ==="
  dc_exec redis redis-cli HGETALL "positions:open"
  echo ""
  echo "=== leaderboard top 20 (sol_sniper:leaderboard) ==="
  dc_exec redis redis-cli ZREVRANGE "sol_sniper:leaderboard" 0 19 WITHSCORES
  echo ""
  echo "=== kill_switch ==="
  dc_exec redis redis-cli GET "kill_switch"
  echo ""
  echo "=== seen:token:* count ==="
  dc_exec redis redis-cli DBSIZE
} > "$OUT_DIR/redis-state.txt" 2>&1 || true

# ── 5. Prometheus metrics snapshot ───────────────────────────────────────────
echo "[capture] Scraping Prometheus metrics..."
# Try the bot's /metrics endpoint directly
curl -sf "http://localhost:8080/metrics" > "$OUT_DIR/prometheus-metrics.txt" 2>/dev/null || \
  curl -sf "http://127.0.0.1:8080/metrics" > "$OUT_DIR/prometheus-metrics.txt" 2>/dev/null || \
  dc_exec sol-sniper-bot wget -qO- "http://localhost:8080/metrics" > "$OUT_DIR/prometheus-metrics.txt" 2>/dev/null || \
  echo "[capture] WARNING: could not scrape /metrics" > "$OUT_DIR/prometheus-metrics.txt"

# ── 6. Training JSONL (archive + rotate so next run starts fresh) ────────────
echo "[capture] Copying and rotating training data..."
JSONL_SRC="./data/sol_sniper_training_v1.jsonl"
if [[ -f "$JSONL_SRC" ]]; then
  cp "$JSONL_SRC" "$OUT_DIR/training.jsonl"
  wc -l "$OUT_DIR/training.jsonl" | awk '{print $1 " records"}' > "$OUT_DIR/training-count.txt"
  echo "[capture] $(cat "$OUT_DIR/training-count.txt") in training.jsonl"
  # Truncate the live file so the next run starts with a clean slate.
  # The bot opens the file with O_APPEND so truncation is safe while running.
  > "$JSONL_SRC"
  echo "[capture] Rotated: $JSONL_SRC truncated"
else
  echo "[capture] WARNING: $JSONL_SRC not found"
fi

# ── 7. Derived analytics from training JSONL ─────────────────────────────────
echo "[capture] Computing label histogram..."
if [[ -f "$OUT_DIR/training.jsonl" ]]; then
  python3 - "$OUT_DIR/training.jsonl" "$OUT_DIR/label-histogram.txt" <<'PYEOF'
import sys, json, collections
path, out = sys.argv[1], sys.argv[2]
counts = collections.Counter()
with open(path) as f:
    for line in f:
        try:
            rec = json.loads(line)
            counts[rec.get("final_label", "UNKNOWN")] += 1
        except Exception:
            pass
total = sum(counts.values())
with open(out, "w") as f:
    for label, n in sorted(counts.items(), key=lambda x: -x[1]):
        f.write(f"{n:6d}  {100*n/total:5.1f}%  {label}\n")
PYEOF
  echo "[capture] Label histogram:"
  cat "$OUT_DIR/label-histogram.txt"
fi

# ── 8. Executions, sells, and monitor errors from bot log ────────────────────
echo "[capture] Extracting structured log subsets..."
if [[ -f "$OUT_DIR/bot.log" ]]; then
  grep -F 'bundle skipped'        "$OUT_DIR/bot.log" > "$OUT_DIR/executions.log" 2>/dev/null || true
  grep -E 'sell skipped|stop-loss|take-profit|position timeout' "$OUT_DIR/bot.log" > "$OUT_DIR/sells.log" 2>/dev/null || true
  grep -F '-32429'                "$OUT_DIR/bot.log" > "$OUT_DIR/monitor-errors.log" 2>/dev/null || true
  echo "[capture] executions: $(wc -l < "$OUT_DIR/executions.log"), sells: $(wc -l < "$OUT_DIR/sells.log"), rate-limit errors: $(wc -l < "$OUT_DIR/monitor-errors.log")"
fi

# ── 9. Sell summary ──────────────────────────────────────────────────────────
echo "[capture] Computing sell summary..."
if [[ -f "$OUT_DIR/sells.log" ]] && [[ -s "$OUT_DIR/sells.log" ]]; then
  python3 - "$OUT_DIR/sells.log" "$OUT_DIR/sell-summary.txt" <<'PYEOF'
import sys, json, collections
path, out = sys.argv[1], sys.argv[2]
triggers = collections.Counter()
rois = []
with open(path) as f:
    for line in f:
        try:
            rec = json.loads(line)
            triggers[rec.get("trigger", rec.get("reason", "unknown"))] += 1
            roi = rec.get("roi_pct", rec.get("pnl_pct", None))
            if roi is not None:
                rois.append(float(roi))
        except Exception:
            pass
with open(out, "w") as f:
    f.write("=== Sell Triggers ===\n")
    for t, n in sorted(triggers.items(), key=lambda x: -x[1]):
        f.write(f"  {n:4d}  {t}\n")
    if rois:
        f.write(f"\n=== ROI Distribution ===\n")
        f.write(f"  count : {len(rois)}\n")
        f.write(f"  mean  : {sum(rois)/len(rois):.1f}%\n")
        f.write(f"  min   : {min(rois):.1f}%\n")
        f.write(f"  max   : {max(rois):.1f}%\n")
        positives = [r for r in rois if r > 0]
        f.write(f"  wins  : {len(positives)} / {len(rois)}\n")
PYEOF
  echo "[capture] Sell summary:"
  cat "$OUT_DIR/sell-summary.txt" 2>/dev/null || true
fi

# ── 10. Archive ───────────────────────────────────────────────────────────────
echo "[capture] Creating archive..."
tar -czf "${TAG}.tar.gz" "$TAG/"
echo "[capture] Done: ${TAG}.tar.gz ($(du -sh "${TAG}.tar.gz" | cut -f1))"
echo "[capture] Directory: $OUT_DIR"
