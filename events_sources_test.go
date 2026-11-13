package main

import (
	"database/sql"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// A finished job is evicted from memory and re-read from sqlite, so the SHAs and digests in the
// result must survive the round trip — and a database written before the sources column existed
// must migrate in place and still read.
func TestResultSourcesAndDigestSurviveThePersistenceRoundTrip(t *testing.T) {
	cfg := Config{ConfigDir: t.TempDir(), ControlCenterURL: "http://127.0.0.1:1", SiteID: "s", NodeName: "n"}
	eb, err := newEventBuffer(cfg, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer eb.close()
	snap := buildPublic{
		ID: "job1", Project: "p", State: BuildCompleted, StartedAt: time.Now().UTC(),
		Revision: "appsha", Contexts: map[string]ContextResult{"kit": {Repo: "https://github.com/o/kit", Ref: "1", Revision: "kitsha"}},
		Services: []ServiceProgress{{Name: "a", Tag: "20260101-000000", Digest: fakeDigest}},
	}
	eb.upsertBuild(snap)
	got, ok, err := eb.getBuild("job1")
	if err != nil || !ok {
		t.Fatalf("getBuild: %v %v", ok, err)
	}
	if got.Revision != "appsha" || got.Contexts["kit"].Revision != "kitsha" || got.Services[0].Digest != fakeDigest {
		t.Fatalf("provenance lost across the round trip: %+v", got)
	}
}

func TestOldDatabaseMigratesAndLegacyRowsStillRead(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "events.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE builds (id TEXT PRIMARY KEY, project TEXT, ref TEXT, version_tag TEXT, registry TEXT,
		state TEXT, services BLOB, progress BLOB, started_at_ns INTEGER, completed_at_ns INTEGER, exit_code INTEGER,
		error TEXT, line_count INTEGER, trigger_key TEXT, updated_at_ns INTEGER NOT NULL);
		INSERT INTO builds VALUES ('old','p','r','v','reg','completed',NULL,NULL,1,2,0,'',0,'api',3);`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	cfg := Config{ConfigDir: dir, ControlCenterURL: "http://127.0.0.1:1", SiteID: "s", NodeName: "n"}
	for i := 0; i < 2; i++ { // twice: the migration must be idempotent across restarts
		eb, err := newEventBuffer(cfg, http.DefaultClient)
		if err != nil {
			t.Fatalf("open #%d: %v", i, err)
		}
		got, ok, err := eb.getBuild("old")
		if err != nil || !ok || got.Revision != "" {
			t.Fatalf("legacy row: %+v %v %v", got, ok, err)
		}
		eb.close()
	}
}
