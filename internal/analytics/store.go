package analytics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// stateFile is the canonical filename inside DataDir.
const stateFile = "state.json"

// dailyDir holds per-day rollup files.
const dailyDir = "daily"

// Store is the disk-backed persistence layer for Recorder snapshots.
type Store struct {
	dir              string
	retentionDays    int
	snapshotInterval time.Duration
	log              *slog.Logger
	r                *Recorder

	// Tracks midnight crossings for daily rollup.
	lastDayWritten string

	stop chan struct{}
	done chan struct{}
}

// NewStore configures the persistence layer. Does not start the background
// ticker — call Start for that.
func NewStore(dir string, retentionDays int, snapshotInterval time.Duration, r *Recorder, log *slog.Logger) *Store {
	return &Store{
		dir:              dir,
		retentionDays:    retentionDays,
		snapshotInterval: snapshotInterval,
		log:              log,
		r:                r,
		stop:             make(chan struct{}),
		done:             make(chan struct{}),
	}
}

// Replay loads ./state.json into the recorder. Returns nil for first-run
// (file absent). On corruption, archives the bad file and starts fresh,
// returning nil — the service is more valuable up with empty stats than down.
func (s *Store) Replay() error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(s.dir, dailyDir), 0o755); err != nil {
		return fmt.Errorf("create daily dir: %w", err)
	}

	path := filepath.Join(s.dir, stateFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		s.log.Info("analytics_state_first_run")
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		s.archiveBroken(path, data, fmt.Errorf("parse: %w", err))
		return nil
	}

	switch {
	case snap.SchemaVersion == 0:
		s.archiveBroken(path, data, errors.New("schema_version missing"))
		return nil
	case snap.SchemaVersion > SchemaVersion:
		// Refuse to start — running with an unknown future schema risks data loss.
		return fmt.Errorf("snapshot schema_version=%d is newer than supported %d; refusing to start",
			snap.SchemaVersion, SchemaVersion)
	case snap.SchemaVersion < SchemaVersion:
		// No migrations defined yet. When you add one, fan out by version.
		s.log.Warn("analytics_state_old_schema",
			"have", snap.SchemaVersion, "want", SchemaVersion)
	}

	warnings := ApplySnapshot(s.r, snap)
	for _, w := range warnings {
		s.log.Warn("analytics_state_replay_warning", "warning", w)
	}
	s.log.Info("analytics_state_replayed",
		"saved_at", snap.SavedAt,
		"total_requests", snap.Totals.Requests)
	return nil
}

// archiveBroken renames the unreadable state file aside and logs loudly.
func (s *Store) archiveBroken(path string, raw []byte, cause error) {
	archive := fmt.Sprintf("%s.broken.%d", path, time.Now().Unix())
	if err := os.WriteFile(archive, raw, 0o600); err != nil {
		s.log.Error("analytics_state_archive_failed", "err", err)
	}
	s.log.Error("analytics_state_corrupt_starting_fresh",
		"cause", cause.Error(),
		"archived_to", archive)
	_ = os.Remove(path)
}

// Persist writes the current state to disk atomically. Tmp file is fsynced
// before rename so a crash mid-write can't leave a half-good state.json.
func (s *Store) Persist() error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	snap := MarshalSnapshot(s.r)
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	path := filepath.Join(s.dir, stateFile)
	tmp := path + ".tmp"

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open tmp: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("fsync tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	s.r.SetLastSnapshotAt(snap.SavedAt)
	return nil
}

// Daily holds the per-day aggregate written when midnight crosses.
type Daily struct {
	Date            string `json:"date"`
	Requests        int64  `json:"requests"`
	PDFs            int64  `json:"pdfs"`
	Failures        int64  `json:"failures"`
	BytesIn         int64  `json:"bytes_in"`
	BytesOut        int64  `json:"bytes_out"`
	LatencyP50Ms    int64  `json:"latency_p50_ms"`
	LatencyP95Ms    int64  `json:"latency_p95_ms"`
	LatencyP99Ms    int64  `json:"latency_p99_ms"`
	PeakRPS         int64  `json:"peak_rps"`
	PeakConcurrency int64  `json:"peak_concurrency"`
	BrowserRestarts int64  `json:"browser_restarts"`
}

// rollupDaily writes yesterday's aggregate file and prunes old files.
// Called from the snapshot ticker when day changes.
func (s *Store) rollupDaily(now time.Time) {
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
	if yesterday == s.lastDayWritten {
		return
	}
	stats := s.r.rolling.Snapshot(now, 24*60)
	hist := s.r.latency.Snapshot()
	peak1m := s.r.rps.Peak1m()
	peakAll, _ := s.r.rps.PeakAll()
	if peak1m > peakAll {
		peak1m = peakAll
	}
	d := Daily{
		Date:            yesterday,
		Requests:        stats.Requests,
		PDFs:            stats.PDFs,
		Failures:        stats.Failures,
		BytesIn:         stats.BytesIn,
		BytesOut:        stats.BytesOut,
		LatencyP50Ms:    hist.Percentile(0.5),
		LatencyP95Ms:    hist.Percentile(0.95),
		LatencyP99Ms:    hist.Percentile(0.99),
		PeakRPS:         peak1m,
		PeakConcurrency: s.r.peakInFlight.Load(),
		BrowserRestarts: s.r.browserRestarts.Load(),
	}
	data, _ := json.MarshalIndent(d, "", "  ")
	path := filepath.Join(s.dir, dailyDir, yesterday+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		s.log.Error("analytics_daily_write_failed", "err", err)
		return
	}
	s.lastDayWritten = yesterday
	s.log.Info("analytics_daily_written", "date", yesterday)
	s.pruneDaily(now)
}

// pruneDaily removes daily files older than retentionDays.
func (s *Store) pruneDaily(now time.Time) {
	entries, err := os.ReadDir(filepath.Join(s.dir, dailyDir))
	if err != nil {
		return
	}
	cutoff := now.AddDate(0, 0, -s.retentionDays).Format("2006-01-02")
	var removed int
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		date := strings.TrimSuffix(name, ".json")
		if date < cutoff {
			_ = os.Remove(filepath.Join(s.dir, dailyDir, name))
			removed++
		}
	}
	if removed > 0 {
		s.log.Info("analytics_daily_pruned", "removed", removed, "cutoff", cutoff)
	}
}

// Start runs the snapshot ticker in the background until Stop is called.
func (s *Store) Start(ctx context.Context) {
	go func() {
		defer close(s.done)
		t := time.NewTicker(s.snapshotInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.stop:
				return
			case now := <-t.C:
				if err := s.Persist(); err != nil {
					s.log.Error("analytics_persist_failed", "err", err)
				}
				s.rollupDaily(now)
			}
		}
	}()
}

// Stop halts the snapshot ticker. Idempotent.
func (s *Store) Stop() {
	select {
	case <-s.stop:
		return
	default:
	}
	close(s.stop)
	<-s.done
}

// ListDailyFiles is a small helper for tests.
func (s *Store) ListDailyFiles() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, dailyDir))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}
