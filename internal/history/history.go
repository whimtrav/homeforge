// Package history persists entity state changes to TimescaleDB and serves them
// back for graphing. Recording is on-change (the entity store only publishes
// entity.state_changed on a real change), batched, and never blocks the hub.
// Retention, columnar compression, and 1m/1h rollups are native Timescale
// policies configured at startup.
package history

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/whimtrav/homeforge/internal/entity"
)

// Config controls the Timescale-backed history recorder. Interval fields are
// Postgres interval literals ("30 days"); "" or "0" means "keep forever".
type Config struct {
	Enabled       bool
	DSN           string
	RetentionRaw  string
	Retention1m   string
	Retention1h   string
	CompressAfter string
}

type sample struct {
	ts  time.Time
	eid string
	val *float64
	str string
}

// Point is one returned history datapoint.
type Point struct {
	TS  time.Time `json:"ts"`
	Val *float64  `json:"val,omitempty"`
	Str string    `json:"str,omitempty"`
}

type History struct {
	pool *pgxpool.Pool
	ch   chan sample
}

// Open connects to Timescale, initialises the schema/policies, and starts the
// batched writer. It retries the initial connection so homeforge can start
// before the timescaledb container is ready.
func Open(ctx context.Context, cfg Config) (*History, error) {
	pool, err := pgxpool.New(ctx, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("pgxpool: %w", err)
	}

	var pingErr error
	for i := 0; i < 30; i++ { // up to ~60s while the DB container boots
		if pingErr = pool.Ping(ctx); pingErr == nil {
			break
		}
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if pingErr != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", pingErr)
	}

	h := &History{pool: pool, ch: make(chan sample, 8192)}
	if err := h.initSchema(ctx, cfg); err != nil {
		pool.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	go h.writer(ctx)
	slog.Info("history: connected to timescaledb, recording enabled")
	return h, nil
}

func (h *History) exec(ctx context.Context, sql string, args ...any) {
	if _, err := h.pool.Exec(ctx, sql, args...); err != nil {
		slog.Warn("history: schema stmt failed", "err", err, "sql", firstLine(sql))
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func (h *History) initSchema(ctx context.Context, cfg Config) error {
	if _, err := h.pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS timescaledb`); err != nil {
		return err
	}
	h.exec(ctx, `CREATE TABLE IF NOT EXISTS states (
		ts        timestamptz      NOT NULL,
		entity_id text             NOT NULL,
		value     double precision,
		str       text
	)`)
	h.exec(ctx, `SELECT create_hypertable('states','ts', if_not_exists => TRUE, chunk_time_interval => INTERVAL '1 day')`)
	h.exec(ctx, `CREATE INDEX IF NOT EXISTS states_entity_ts ON states (entity_id, ts DESC)`)

	// 1-minute and 1-hour rollups (real-time continuous aggregates).
	h.exec(ctx, `CREATE MATERIALIZED VIEW IF NOT EXISTS states_1m
		WITH (timescaledb.continuous) AS
		SELECT time_bucket('1 minute', ts) AS bucket, entity_id,
		       avg(value) AS avg, min(value) AS min, max(value) AS max, last(str, ts) AS str
		FROM states GROUP BY bucket, entity_id
		WITH NO DATA`)
	h.exec(ctx, `CREATE MATERIALIZED VIEW IF NOT EXISTS states_1h
		WITH (timescaledb.continuous) AS
		SELECT time_bucket('1 hour', ts) AS bucket, entity_id,
		       avg(value) AS avg, min(value) AS min, max(value) AS max, last(str, ts) AS str
		FROM states GROUP BY bucket, entity_id
		WITH NO DATA`)
	h.exec(ctx, `SELECT add_continuous_aggregate_policy('states_1m',
		start_offset => INTERVAL '3 hours', end_offset => INTERVAL '1 minute',
		schedule_interval => INTERVAL '1 minute', if_not_exists => TRUE)`)
	h.exec(ctx, `SELECT add_continuous_aggregate_policy('states_1h',
		start_offset => INTERVAL '3 days', end_offset => INTERVAL '1 hour',
		schedule_interval => INTERVAL '1 hour', if_not_exists => TRUE)`)

	// Columnar compression on raw chunks older than CompressAfter.
	if iv := cfg.CompressAfter; iv != "" && iv != "0" {
		h.exec(ctx, `ALTER TABLE states SET (
			timescaledb.compress,
			timescaledb.compress_segmentby = 'entity_id',
			timescaledb.compress_orderby = 'ts DESC')`)
		h.exec(ctx, `SELECT add_compression_policy('states', $1::interval, if_not_exists => TRUE)`, iv)
	}

	// Retention ("" / "0" == keep forever).
	addRetention := func(tbl, iv string) {
		if iv == "" || iv == "0" {
			return
		}
		h.exec(ctx, fmt.Sprintf(`SELECT add_retention_policy('%s', $1::interval, if_not_exists => TRUE)`, tbl), iv)
	}
	addRetention("states", cfg.RetentionRaw)
	addRetention("states_1m", cfg.Retention1m)
	addRetention("states_1h", cfg.Retention1h)
	return nil
}

// Record queues an entity's current state. Numeric states store a float value;
// on/off-style states store both a 0/1 value (so they graph) and the raw string.
func (h *History) Record(e entity.Entity) {
	s := sample{ts: e.LastUpdated, eid: e.ID}
	if s.ts.IsZero() {
		s.ts = time.Now()
	}
	state := strings.TrimSpace(e.State)
	if f, err := strconv.ParseFloat(state, 64); err == nil {
		s.val = &f
	} else {
		switch strings.ToLower(state) {
		case "on", "true", "open", "home", "detected", "yes":
			v := 1.0
			s.val = &v
		case "off", "false", "closed", "away", "clear", "no":
			v := 0.0
			s.val = &v
		}
		s.str = e.State
	}
	select {
	case h.ch <- s:
	default: // buffer full — drop rather than stall the hub for history
	}
}

func (h *History) writer(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	buf := make([]sample, 0, 512)
	flush := func() {
		if len(buf) == 0 {
			return
		}
		rows := make([][]any, len(buf))
		for i, s := range buf {
			var v, str any
			if s.val != nil {
				v = *s.val
			}
			if s.str != "" {
				str = s.str
			}
			rows[i] = []any{s.ts, s.eid, v, str}
		}
		if _, err := h.pool.CopyFrom(ctx, pgx.Identifier{"states"},
			[]string{"ts", "entity_id", "value", "str"}, pgx.CopyFromRows(rows)); err != nil {
			slog.Warn("history: flush failed", "err", err, "rows", len(buf))
		}
		buf = buf[:0]
	}
	for {
		select {
		case <-ctx.Done():
			flush()
			return
		case s := <-h.ch:
			buf = append(buf, s)
			if len(buf) >= 500 {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// Query returns history for an entity between start and end. resolution is
// "raw", "1m", or "1h"; empty auto-selects based on the requested range.
func (h *History) Query(ctx context.Context, eid string, start, end time.Time, resolution string) ([]Point, error) {
	if resolution == "" {
		switch d := end.Sub(start); {
		case d <= 6*time.Hour:
			resolution = "raw"
		case d <= 14*24*time.Hour:
			resolution = "1m"
		default:
			resolution = "1h"
		}
	}
	var q string
	switch resolution {
	case "1m":
		q = `SELECT bucket, avg, str FROM states_1m WHERE entity_id=$1 AND bucket BETWEEN $2 AND $3 ORDER BY bucket`
	case "1h":
		q = `SELECT bucket, avg, str FROM states_1h WHERE entity_id=$1 AND bucket BETWEEN $2 AND $3 ORDER BY bucket`
	default:
		resolution = "raw"
		q = `SELECT ts, value, str FROM states WHERE entity_id=$1 AND ts BETWEEN $2 AND $3 ORDER BY ts`
	}
	rows, err := h.pool.Query(ctx, q, eid, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Point{}
	for rows.Next() {
		var p Point
		var val *float64
		var str *string
		if err := rows.Scan(&p.TS, &val, &str); err != nil {
			return nil, err
		}
		p.Val = val
		if str != nil {
			p.Str = *str
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// IntegrateFlow integrates a flow-rate series (units/min) over [start,end] via
// trapezoidal integration with carry-in: the value in effect at start (the last
// recorded sample at or before start) is projected forward to start, so a flow that
// spans the window boundary is still counted. Intervals longer than maxGap are NOT
// integrated across — active flow reports every few seconds, so a long gap means the
// flow had stopped (an idle gap holds value 0 → contributes nothing anyway; a stale
// non-zero value must not be projected across a long silence). Returns units·min, so
// for a gal/min series the result is gallons.
func (h *History) IntegrateFlow(ctx context.Context, eid string, start, end time.Time) (float64, error) {
	const maxGap = 120 * time.Second
	type pt struct {
		ts time.Time
		v  float64
	}
	var pts []pt

	// carry-in: last sample at/before start, projected to the window start
	var cts time.Time
	var cv float64
	err := h.pool.QueryRow(ctx,
		`SELECT ts, value FROM states WHERE entity_id=$1 AND ts<=$2 AND value IS NOT NULL ORDER BY ts DESC LIMIT 1`,
		eid, start).Scan(&cts, &cv)
	if err == nil {
		pts = append(pts, pt{start, cv})
	} else if err != pgx.ErrNoRows {
		return 0, err
	}

	rows, err := h.pool.Query(ctx,
		`SELECT ts, value FROM states WHERE entity_id=$1 AND ts>$2 AND ts<=$3 AND value IS NOT NULL ORDER BY ts`,
		eid, start, end)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var p pt
		if err := rows.Scan(&p.ts, &p.v); err != nil {
			return 0, err
		}
		pts = append(pts, p)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	total := 0.0
	for i := 1; i < len(pts); i++ {
		dt := pts[i].ts.Sub(pts[i-1].ts)
		if dt <= 0 || dt > maxGap {
			continue
		}
		total += (pts[i-1].v + pts[i].v) / 2 * dt.Minutes()
	}
	return total, nil
}

// CycleTotal sums the POSITIVE deltas of a monotonic (total_increasing) counter over
// [start,end], ignoring resets — a drop = counter/inverter reset, not real usage. This is
// how a utility_meter derives per-cycle usage from a cumulative kWh counter.
func (h *History) CycleTotal(ctx context.Context, eid string, start, end time.Time) (float64, error) {
	var prev float64
	havePrev := false
	var cts time.Time
	err := h.pool.QueryRow(ctx,
		`SELECT ts, value FROM states WHERE entity_id=$1 AND ts<=$2 AND value IS NOT NULL ORDER BY ts DESC LIMIT 1`,
		eid, start).Scan(&cts, &prev)
	if err == nil {
		havePrev = true
	} else if err != pgx.ErrNoRows {
		return 0, err
	}
	rows, err := h.pool.Query(ctx,
		`SELECT value FROM states WHERE entity_id=$1 AND ts>$2 AND ts<=$3 AND value IS NOT NULL ORDER BY ts`,
		eid, start, end)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	total := 0.0
	for rows.Next() {
		var v float64
		if err := rows.Scan(&v); err != nil {
			return 0, err
		}
		if havePrev {
			if d := v - prev; d > 0 {
				total += d
			}
		}
		prev, havePrev = v, true
	}
	return total, rows.Err()
}

func (h *History) Close() {
	if h.pool != nil {
		h.pool.Close()
	}
}
