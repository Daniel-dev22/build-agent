package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	eventoutbox "github.com/Daniel-dev22/agent-kit-go/eventoutbox"
	_ "modernc.org/sqlite" // pure-Go sqlite driver, works with CGO_ENABLED=0
)

// EventPayload is the body POSTed to the controller on every build lifecycle
// transition. The controller is expected to deserialize this exact shape.
type EventPayload struct {
	JobID       string            `json:"job_id"`
	Site        string            `json:"site"`
	Node        string            `json:"node"`
	Project     string            `json:"project"`
	Ref         string            `json:"ref,omitempty"`
	VersionTag  string            `json:"version_tag,omitempty"`
	Registry    string            `json:"registry,omitempty"`
	State       BuildState        `json:"state"`
	Event       JobEvent          `json:"event"`
	Services    []ServiceProgress `json:"services,omitempty"`
	StartedAt   *time.Time        `json:"started_at,omitempty"`
	CompletedAt *time.Time        `json:"completed_at,omitempty"`
	ExitCode    int               `json:"exit_code"`
	ErrorMsg    string            `json:"error,omitempty"`
	LineCount   int               `json:"line_count"`
	TriggerKey  string            `json:"trigger_key,omitempty"`
	EmittedAt   time.Time         `json:"emitted_at"`
}

// eventBuffer owns the agent's events.sqlite: the durable outbound queue (kit
// eventoutbox, owns pending_events) plus a local builds table that gives the
// fleet snapshot restart-survival (the in-memory registry starts empty after a
// restart; this retains recent history for the dashboard + the orphan sweep).
type eventBuffer struct {
	cfg    Config
	db     *sql.DB
	outbox *eventoutbox.Outbox
}

func newEventBuffer(cfg Config, client *http.Client) (*eventBuffer, error) {
	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir config dir: %w", err)
	}
	dbPath := filepath.Join(cfg.ConfigDir, "events.sqlite")
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS builds (
			id              TEXT PRIMARY KEY,
			project         TEXT,
			ref             TEXT,
			version_tag     TEXT,
			registry        TEXT,
			state           TEXT,
			services        BLOB,
			progress        BLOB,
			started_at_ns   INTEGER,
			completed_at_ns INTEGER,
			exit_code       INTEGER,
			error           TEXT,
			line_count      INTEGER,
			trigger_key     TEXT,
			updated_at_ns   INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_builds_updated ON builds (updated_at_ns);
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	urlFor := func(jobID string) string {
		return cfg.ControlCenterURL + "/api/build/jobs/" + jobID + "/event"
	}
	outbox, err := eventoutbox.New(db, client, urlFor)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("event outbox: %w", err)
	}
	return &eventBuffer{cfg: cfg, db: db, outbox: outbox}, nil
}

func (e *eventBuffer) DB() *sql.DB {
	if e == nil {
		return nil
	}
	return e.db
}

func (e *eventBuffer) Start(ctx context.Context) {
	if e != nil {
		e.outbox.Start(ctx)
	}
}

func (e *eventBuffer) close() {
	if e == nil {
		return
	}
	e.outbox.Close()
	if e.db != nil {
		_ = e.db.Close()
	}
}

// handleJobEvent is the JobEventHook: persist to the local builds table
// (restart-survival) and enqueue for durable delivery to the controller.
func (e *eventBuffer) handleJobEvent(b *Build, evt JobEvent) {
	snap := b.snapshot()
	payload := EventPayload{
		JobID:       snap.ID,
		Site:        e.cfg.SiteID,
		Node:        e.cfg.NodeName,
		Project:     snap.Project,
		Ref:         snap.Ref,
		VersionTag:  snap.VersionTag,
		Registry:    snap.Registry,
		State:       snap.State,
		Event:       evt,
		Services:    snap.Services,
		StartedAt:   tPtr(snap.StartedAt),
		CompletedAt: tPtr(snap.CompletedAt),
		ExitCode:    snap.ExitCode,
		ErrorMsg:    snap.ErrorMsg,
		LineCount:   snap.LineCount,
		TriggerKey:  snap.TriggerKey,
		EmittedAt:   time.Now().UTC(),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		slog.Error("marshal event payload failed", "error", err, "job", snap.ID)
		return
	}
	e.upsertBuild(snap)
	e.outbox.Enqueue(snap.ID, string(evt), body)
}

func tsToNs(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func nsToTime(ns int64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns).UTC()
}

func (e *eventBuffer) upsertBuild(snap buildPublic) {
	if e == nil || e.db == nil {
		return
	}
	servicesJSON, _ := json.Marshal(snap.Services)
	var progressJSON []byte
	if snap.Progress != nil {
		progressJSON, _ = json.Marshal(snap.Progress)
	}
	_, err := e.db.Exec(`
		INSERT INTO builds (id, project, ref, version_tag, registry, state, services, progress,
			started_at_ns, completed_at_ns, exit_code, error, line_count, trigger_key, updated_at_ns)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			project=excluded.project, ref=excluded.ref, version_tag=excluded.version_tag,
			registry=excluded.registry, state=excluded.state, services=excluded.services,
			progress=excluded.progress, started_at_ns=excluded.started_at_ns,
			completed_at_ns=excluded.completed_at_ns, exit_code=excluded.exit_code,
			error=excluded.error, line_count=excluded.line_count, trigger_key=excluded.trigger_key,
			updated_at_ns=excluded.updated_at_ns`,
		snap.ID, snap.Project, snap.Ref, snap.VersionTag, snap.Registry, string(snap.State),
		servicesJSON, progressJSON, tsToNs(snap.StartedAt), tsToNs(snap.CompletedAt),
		snap.ExitCode, snap.ErrorMsg, snap.LineCount, snap.TriggerKey, time.Now().UnixNano(),
	)
	if err != nil {
		slog.Warn("persist build failed", "error", err, "job", snap.ID)
	}
}

const buildSelectCols = `id, project, ref, version_tag, registry, state, services, progress,
	started_at_ns, completed_at_ns, exit_code, error, line_count, trigger_key`

func scanBuild(s interface{ Scan(...any) error }) (buildPublic, error) {
	var (
		bp                     buildPublic
		state                  string
		services, progress     []byte
		startedNs, completedNs int64
	)
	if err := s.Scan(&bp.ID, &bp.Project, &bp.Ref, &bp.VersionTag, &bp.Registry, &state,
		&services, &progress, &startedNs, &completedNs, &bp.ExitCode, &bp.ErrorMsg,
		&bp.LineCount, &bp.TriggerKey); err != nil {
		return buildPublic{}, err
	}
	bp.State = BuildState(state)
	bp.StartedAt = nsToTime(startedNs)
	bp.CompletedAt = nsToTime(completedNs)
	if len(services) > 0 {
		_ = json.Unmarshal(services, &bp.Services)
	}
	if len(progress) > 0 {
		var p BuildProgress
		if json.Unmarshal(progress, &p) == nil {
			bp.Progress = &p
		}
	}
	return bp, nil
}

func (e *eventBuffer) listRecentBuilds(limit int) ([]buildPublic, error) {
	if e == nil || e.db == nil {
		return nil, nil
	}
	rows, err := e.db.Query(`SELECT `+buildSelectCols+` FROM builds ORDER BY updated_at_ns DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []buildPublic
	for rows.Next() {
		bp, err := scanBuild(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, bp)
	}
	return out, rows.Err()
}

func (e *eventBuffer) getBuild(id string) (buildPublic, bool, error) {
	if e == nil || e.db == nil {
		return buildPublic{}, false, nil
	}
	bp, err := scanBuild(e.db.QueryRow(`SELECT `+buildSelectCols+` FROM builds WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return buildPublic{}, false, nil
	}
	if err != nil {
		return buildPublic{}, false, err
	}
	return bp, true, nil
}
