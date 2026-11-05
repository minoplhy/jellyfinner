package sqlite

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"jellyfinner/internal/database"
	"jellyfinner/internal/model"
)

func TestMigrateRunsOnceAndInOrder(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"), database.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// The store already has the real migrations, so the test steps follow them.
	steps := append(append([]string{}, migrations...), `CREATE TABLE m1 (x INTEGER)`, `ALTER TABLE m1 ADD COLUMN y INTEGER`)
	first := len(migrations) + 1
	if err := migrate(s.db, steps[:first]); err != nil {
		t.Fatal(err)
	}
	// Running a step twice would fail, so a pass proves only the new one ran.
	if err := migrate(s.db, steps); err != nil {
		t.Fatal(err)
	}
	if err := migrate(s.db, steps); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO m1 (x, y) VALUES (1, 2)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(s.db, steps[:first]); err == nil {
		t.Error("an older program must refuse a newer database")
	}
}

// TestMigrateNodesToIDs upgrades a version 3 database, where everything named
// nodes by name, and checks every reference moved to an ID.
func TestMigrateNodesToIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db, migrations[:3]); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO nodes (name, kind, token_hash, allow, created, paused) VALUES ('worker1', 'worker', 'h1', '10.0.0.0/8', 1, 1)`,
		`INSERT INTO nodes (name, kind, token_hash, url, quota_bytes, signing_key, created) VALUES ('edge1', 'edge', 'h2', 'https://e', 5, 'k', 1)`,
		`INSERT INTO nodes (name, kind, token_hash, created) VALUES ('cloud-p-aa', 'worker', 'h3', 1)`,
		`INSERT INTO jobs (id, path, output_path, source_size, source_mtime, revision, priority, state, node, created, updated)
			VALUES ('j1', 'a', 'a', 1, 1, 1, 1, 'done', 'worker1', 1, 1), ('j2', 'b', 'b', 1, 1, 1, 1, 'done', 'gone-worker', 1, 1),
			       ('j3', 'c', 'c', 1, 1, 1, 1, 'queued', '', 1, 1), ('j4', 'd', 'd', 1, 1, 1, 1, 'done', 'cloud-p-old', 1, 1)`,
		`INSERT INTO placements (node, sha256, output, size, state) VALUES ('edge1', 'aa', 'a', 1, 'active')`,
		`INSERT INTO edge_status (node, last_seen) VALUES ('edge1', 7)`,
		`INSERT INTO cloud_servers (provider, id, name, type, location, state, created) VALUES
			('p', '1', 'cloud-p-aa', 't', 'l', 'ready', 1), ('p', '2', 'cloud-p-old', 't', 'l', 'deleted', 1), ('p', '3', 'cloud-p-nojobs', 't', 'l', 'deleted', 1)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()

	s, err := Open(path, database.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	live, err := s.LoadNodes()
	if err != nil || len(live) != 3 {
		t.Fatalf("live nodes = %+v, %v", live, err)
	}
	ids := map[string]string{}
	for _, n := range live {
		if len(n.ID) != 16 {
			t.Errorf("%s has ID %q", n.Name, n.ID)
		}
		ids[n.Name] = n.ID
	}
	if w := live[2]; w.Name != "worker1" || !w.Paused || w.TokenHash != "h1" || len(w.Allow) != 1 {
		t.Errorf("worker1 lost settings: %+v", w)
	}
	if e := live[1]; e.Name != "edge1" || e.URL != "https://e" || e.QuotaBytes != 5 || e.SigningKey != "k" {
		t.Errorf("edge1 lost settings: %+v", e)
	}

	names, err := s.NodeNames()
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"j1": "worker1", "j2": "gone-worker", "j4": "cloud-p-old"} {
		j, err := s.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if j.NodeName != want || names[j.Node] != want || j.Node == want {
			t.Errorf("%s: node %q, name %q, want an ID named %s", id, j.Node, j.NodeName, want)
		}
	}
	if j, _ := s.Get("j3"); j.Node != "" {
		t.Errorf("an unclaimed job got node %q", j.Node)
	}
	if j, _ := s.Get("j1"); j.Node != ids["worker1"] {
		t.Errorf("j1 points at %q, not worker1's ID", j.Node)
	}
	if p, err := s.NodePlacements(ids["edge1"]); err != nil || len(p) != 1 {
		t.Errorf("edge1 placements = %+v, %v", p, err)
	}
	if st, err := s.GetNodeStatus(ids["edge1"]); err != nil || st.LastSeen != 7 {
		t.Errorf("edge1 status = %+v, %v", st, err)
	}
	servers, err := s.CloudServers(time.Unix(0, 0))
	if err != nil || len(servers) != 3 {
		t.Fatalf("cloud servers = %+v, %v", servers, err)
	}
	for _, srv := range servers {
		if names[srv.Node] != srv.Name {
			t.Errorf("server %s points at node %q, named %q", srv.Name, srv.Node, names[srv.Node])
		}
	}
	if servers[0].Node != ids["cloud-p-aa"] {
		t.Errorf("the live server's node is %q, not its live node", servers[0].Node)
	}
	// Removed names can be taken again; live ones cannot.
	if err := s.AddNode(&model.Node{ID: "new", Name: "gone-worker", Kind: model.NodeWorker, TokenHash: "x"}); err != nil {
		t.Errorf("reusing a removed name: %v", err)
	}
	if err := s.AddNode(&model.Node{ID: "new2", Name: "worker1", Kind: model.NodeWorker, TokenHash: "y"}); !errors.Is(err, model.ErrNodeExists) {
		t.Errorf("taking a live name: %v", err)
	}
}
