package team

import (
	"testing"
	"time"
)

// newTestAppWithDB creates an html-only app (no server-side build) and returns
// the store rooted at dir plus the new app id, ready for db ops.
func newTestAppWithDB(t *testing.T, dir string) (*customAppStore, string) {
	t.Helper()
	store := newCustomAppStore(dir)
	now := time.Unix(1_700_000_000, 0).UTC()
	app, err := store.Save(CustomAppWriteRequest{
		Name:  "Data App",
		HTML:  validAppHTML,
		Actor: "app-builder",
	}, now)
	if err != nil {
		t.Fatalf("Save create: %v", err)
	}
	return store, app.ID
}

func TestAppDBDefineUpsertQueryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, id := newTestAppWithDB(t, dir)

	// Empty app: no tables yet.
	tables, err := store.AppDBTables(id)
	if err != nil {
		t.Fatalf("AppDBTables empty: %v", err)
	}
	if len(tables) != 0 {
		t.Fatalf("want 0 tables on fresh app, got %d", len(tables))
	}

	// Upsert to an UNDEFINED table auto-defines it from the row shape (the
	// "just works" path so an authored tool can create its own data). Use a
	// throwaway table so the "Emails" flow below stays a clean define-first case.
	if auto, err := store.UpsertAppDBRows(id, "AutoTbl", []map[string]any{{"id": "1", "n": 5}}, "id"); err != nil {
		t.Fatalf("upsert auto-define: want success, got error %v", err)
	} else if len(auto.Columns) != 2 || len(auto.Rows) != 1 {
		t.Fatalf("auto-define shape = %d cols / %d rows, want 2/1", len(auto.Columns), len(auto.Rows))
	}

	// Define with a bad/empty type normalizes to string; a duplicate column dedups.
	def, err := store.DefineAppDBTable(id, "Emails", []AppDBColumn{
		{Name: "id", Type: "string"},
		{Name: "id", Type: "string"},        // duplicate -> dropped
		{Name: "urgency", Type: "number"},   // kept
		{Name: "flagged", Type: "nonsense"}, // -> string
	})
	if err != nil {
		t.Fatalf("DefineAppDBTable: %v", err)
	}
	if len(def.Columns) != 3 {
		t.Fatalf("want 3 columns after dedup, got %d (%v)", len(def.Columns), def.Columns)
	}
	if def.Columns[2].Type != "string" {
		t.Fatalf("bad type should normalize to string, got %q", def.Columns[2].Type)
	}

	// Upsert two rows, then upsert one with the SAME key -> replace, not append.
	if _, err := store.UpsertAppDBRows(id, "Emails", []map[string]any{
		{"id": "a", "urgency": 10},
		{"id": "b", "urgency": 20},
	}, "id"); err != nil {
		t.Fatalf("upsert initial: %v", err)
	}
	tbl, err := store.UpsertAppDBRows(id, "Emails", []map[string]any{
		{"id": "a", "urgency": 99}, // replaces a
		{"id": "c", "urgency": 30}, // appends c
	}, "id")
	if err != nil {
		t.Fatalf("upsert dedup: %v", err)
	}
	if len(tbl.Rows) != 3 {
		t.Fatalf("want 3 rows after key dedup, got %d (%v)", len(tbl.Rows), tbl.Rows)
	}
	// Row "a" must carry the replaced value.
	var found bool
	for _, row := range tbl.Rows {
		if row["id"] == "a" {
			found = true
			// In-memory the value is the native int 99; after a disk round-trip it
			// is float64(99). Compare via the string form to cover both.
			if got := appDBKeyValue(row["urgency"]); got != "99" {
				t.Fatalf("row a urgency = %v, want 99", row["urgency"])
			}
		}
	}
	if !found {
		t.Fatalf("row a missing after dedup upsert")
	}

	// Query returns the same table.
	q, err := store.QueryAppDBTable(id, "Emails")
	if err != nil {
		t.Fatalf("QueryAppDBTable: %v", err)
	}
	if len(q.Rows) != 3 || len(q.Columns) != 3 {
		t.Fatalf("query shape = %d cols / %d rows, want 3/3", len(q.Columns), len(q.Rows))
	}

	// Persistence: a fresh store over the same root reads the same rows.
	reopened := newCustomAppStore(dir)
	rq, err := reopened.QueryAppDBTable(id, "Emails")
	if err != nil {
		t.Fatalf("reopened query: %v", err)
	}
	if len(rq.Rows) != 3 {
		t.Fatalf("reopened rows = %d, want 3 (persistence lost)", len(rq.Rows))
	}

	// Clear empties rows, keeps columns.
	cleared, err := store.ClearAppDBTable(id, "Emails")
	if err != nil {
		t.Fatalf("ClearAppDBTable: %v", err)
	}
	if len(cleared.Rows) != 0 || len(cleared.Columns) != 3 {
		t.Fatalf("cleared = %d rows / %d cols, want 0/3", len(cleared.Rows), len(cleared.Columns))
	}
}

func TestAppDBKeylessUpsertAppends(t *testing.T) {
	store, id := newTestAppWithDB(t, t.TempDir())
	if _, err := store.DefineAppDBTable(id, "Log", []AppDBColumn{{Name: "msg", Type: "string"}}); err != nil {
		t.Fatalf("define: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := store.UpsertAppDBRows(id, "Log", []map[string]any{{"msg": "x"}}, ""); err != nil {
			t.Fatalf("keyless upsert %d: %v", i, err)
		}
	}
	tbl, err := store.QueryAppDBTable(id, "Log")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(tbl.Rows) != 3 {
		t.Fatalf("keyless upsert should append, got %d rows want 3", len(tbl.Rows))
	}
}

func TestAppDBUpsertRejectsUnknownKey(t *testing.T) {
	store, id := newTestAppWithDB(t, t.TempDir())
	if _, err := store.DefineAppDBTable(id, "Emails", []AppDBColumn{{Name: "id", Type: "string"}}); err != nil {
		t.Fatalf("define: %v", err)
	}
	// A misspelled key must be a caller error, not silently treated as "" for
	// every row (which would collapse unrelated rows onto one key).
	if _, err := store.UpsertAppDBRows(id, "Emails", []map[string]any{{"id": "a"}}, "idd"); err == nil || !isCustomAppCallerError(err) {
		t.Fatalf("unknown key: want caller error, got %v", err)
	}
	// The rejected upsert persisted nothing.
	tbl, err := store.QueryAppDBTable(id, "Emails")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(tbl.Rows) != 0 {
		t.Fatalf("rejected upsert must not persist rows, got %d", len(tbl.Rows))
	}
}

func TestAppDBUpsertRejectsRowMissingKey(t *testing.T) {
	store, id := newTestAppWithDB(t, t.TempDir())
	if _, err := store.DefineAppDBTable(id, "Emails", []AppDBColumn{
		{Name: "id", Type: "string"},
		{Name: "subject", Type: "string"},
	}); err != nil {
		t.Fatalf("define: %v", err)
	}
	if _, err := store.UpsertAppDBRows(id, "Emails", []map[string]any{{"id": "a", "subject": "s1"}}, "id"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A row omitting the key column, and a row whose key value is empty, are
	// both invalid: pre-fix they dedup to the "" key and overwrite each other.
	for _, rows := range [][]map[string]any{
		{{"subject": "no key at all"}},
		{{"id": "", "subject": "empty key"}},
	} {
		if _, err := store.UpsertAppDBRows(id, "Emails", rows, "id"); err == nil || !isCustomAppCallerError(err) {
			t.Fatalf("rows %v: want caller error, got %v", rows, err)
		}
	}
	// The seeded row is untouched by the rejected upserts.
	tbl, err := store.QueryAppDBTable(id, "Emails")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(tbl.Rows) != 1 || tbl.Rows[0]["subject"] != "s1" {
		t.Fatalf("seed row must be untouched, got %v", tbl.Rows)
	}
}

func TestAppDBUnknownAppIsCallerError(t *testing.T) {
	store := newCustomAppStore(t.TempDir())
	ghost := "app_0123456789abcdef"
	if _, err := store.AppDBTables(ghost); err == nil || !isCustomAppCallerError(err) {
		t.Fatalf("AppDBTables on ghost app: want caller error, got %v", err)
	}
	if _, err := store.DefineAppDBTable(ghost, "T", []AppDBColumn{{Name: "c"}}); err == nil || !isCustomAppCallerError(err) {
		t.Fatalf("Define on ghost app: want caller error, got %v", err)
	}
}

func TestAppDBTableLookupIsCaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	store, id := newTestAppWithDB(t, dir)
	if _, err := store.DefineAppDBTable(id, "Incidents", []AppDBColumn{{Name: "id", Type: "string"}}); err != nil {
		t.Fatalf("define: %v", err)
	}
	if _, err := store.UpsertAppDBRows(id, "Incidents", []map[string]any{{"id": "INC-1"}}, "id"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// A tool authored with a different case ("incidents") must still find the
	// rows the app wrote to "Incidents" — the app UI and the tool are separate
	// model calls that do not reliably agree on casing.
	q, err := store.QueryAppDBTable(id, "incidents")
	if err != nil {
		t.Fatalf("case-insensitive query: %v", err)
	}
	if len(q.Rows) != 1 {
		t.Fatalf("want 1 row via lowercase name, got %d", len(q.Rows))
	}
	// Upsert with a different case must land in the SAME table, not create a
	// second one.
	if _, err := store.UpsertAppDBRows(id, "INCIDENTS", []map[string]any{{"id": "INC-2"}}, "id"); err != nil {
		t.Fatalf("case-insensitive upsert: %v", err)
	}
	tables, _ := store.AppDBTables(id)
	if len(tables) != 1 {
		t.Fatalf("case variants created %d tables, want 1", len(tables))
	}
	if q2, _ := store.QueryAppDBTable(id, "Incidents"); len(q2.Rows) != 2 {
		t.Fatalf("want 2 rows after case-variant upsert, got %d", len(q2.Rows))
	}
}

func TestInferAppDBColumns(t *testing.T) {
	// Types come from the first non-nil value; "id" sorts first, the rest
	// alphabetically. A JSON round-trip makes numbers float64 and arrays []any.
	rows := []map[string]any{
		{"summary": "text", "id": "b1", "breachCount": float64(3), "customers": []any{"A", "B"}},
		{"id": "b2", "summary": "more"},
	}
	cols := inferAppDBColumns(rows)
	if len(cols) != 4 {
		t.Fatalf("want 4 columns, got %d (%v)", len(cols), cols)
	}
	if cols[0].Name != "id" {
		t.Fatalf("id must sort first, got %q", cols[0].Name)
	}
	byName := map[string]string{}
	for _, c := range cols {
		byName[c.Name] = c.Type
	}
	if byName["breachCount"] != "number" {
		t.Fatalf("breachCount type = %q, want number", byName["breachCount"])
	}
	if byName["customers"] != "string[]" {
		t.Fatalf("customers type = %q, want string[]", byName["customers"])
	}
	if byName["summary"] != "string" {
		t.Fatalf("summary type = %q, want string", byName["summary"])
	}
}

func TestUpsertAutoDefinesMissingTable(t *testing.T) {
	store, id := newTestAppWithDB(t, t.TempDir())
	// An authored tool writes to a table that was never defined; the store
	// creates it from the row instead of erroring, so the write just lands.
	tbl, err := store.UpsertAppDBRows(id, "Briefings", []map[string]any{
		{"id": "b1", "breachCount": float64(2), "summary": "two breaches"},
	}, "id")
	if err != nil {
		t.Fatalf("auto-define upsert: %v", err)
	}
	if len(tbl.Rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(tbl.Rows))
	}
	if got := appDBKeyValue(tbl.Rows[0]["breachCount"]); got != "2" {
		t.Fatalf("breachCount = %v, want 2", tbl.Rows[0]["breachCount"])
	}
	// The inferred schema persists and is queryable by later calls.
	q, err := store.QueryAppDBTable(id, "Briefings")
	if err != nil {
		t.Fatalf("query auto-defined table: %v", err)
	}
	if len(q.Columns) != 3 {
		t.Fatalf("want 3 inferred columns, got %d (%v)", len(q.Columns), q.Columns)
	}
}

func TestInvalidateAppKnowledgeDropsCache(t *testing.T) {
	store, id := newTestAppWithDB(t, t.TempDir())
	if err := store.WriteAppKnowledge(id, []appKnowledgePage{{ID: "p1", Title: "Guide"}}); err != nil {
		t.Fatalf("WriteAppKnowledge: %v", err)
	}
	if _, ok, err := store.ReadAppKnowledge(id); err != nil || !ok {
		t.Fatalf("cache should exist before invalidate: ok=%v err=%v", ok, err)
	}
	if err := store.InvalidateAppKnowledge(id); err != nil {
		t.Fatalf("InvalidateAppKnowledge: %v", err)
	}
	if _, ok, err := store.ReadAppKnowledge(id); err != nil || ok {
		t.Fatalf("cache should be gone after invalidate: ok=%v err=%v", ok, err)
	}
	// Invalidating an already-absent cache is not an error.
	if err := store.InvalidateAppKnowledge(id); err != nil {
		t.Fatalf("second invalidate should be a no-op, got %v", err)
	}
}
