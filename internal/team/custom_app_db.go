package team

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// custom_app_db.go — the per-app typed database. Every custom app gets a small,
// real, persisted store: named tables, typed columns, and rows. The app writes
// its DERIVED data model into it (via the Bridge db.* APIs) and renders FROM it;
// the Data tab reads it directly. This replaces the earlier AI-reconstructed
// "data model" view — the model is no longer guessed, it is the app's actual
// backing store.
//
// Storage: <app_dir>/db.json = {"tables":{"<name>":{"columns":[...],"rows":[...]}}}.
// Guarded by the app store's mutex (db ops are quick JSON reads/writes, so they
// serialize with the store without starving it the way a build would).

const customAppDBFile = "db.json"

// appDBColumnLimit / appDBRowLimit / appDBTableLimit bound a single app's store
// so a runaway app (e.g. re-upserting on every mount without a key) cannot grow
// db.json without limit. Generous for real models; a hard backstop, not a quota.
const (
	appDBTableLimit  = 32
	appDBColumnLimit = 64
	appDBRowLimit    = 5000
)

// appDBColumnType is the closed set of column types an app may declare. Unknown
// or empty types normalize to "string" — the store never rejects a define for a
// bad type, it just falls back, so the app always gets a usable table.
var appDBColumnTypes = map[string]bool{
	"string":   true,
	"number":   true,
	"boolean":  true,
	"date":     true,
	"string[]": true,
}

// AppDBColumn is a typed column in an app table. JSON shape matches the Bridge
// and the Data tab: {"name","type"}.
type AppDBColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// AppDBTable is one table as returned to callers: its name, its typed columns,
// and its rows. Rows are free-form JSON objects keyed by column name.
type AppDBTable struct {
	Name    string           `json:"name"`
	Columns []AppDBColumn    `json:"columns"`
	Rows    []map[string]any `json:"rows"`
}

// appDB is the on-disk shape: a map of table-name -> table body. The name lives
// in the map key, not the body, so a rename is a single key move.
type appDB struct {
	Tables map[string]appDBTableBody `json:"tables"`
}

type appDBTableBody struct {
	Columns []AppDBColumn    `json:"columns"`
	Rows    []map[string]any `json:"rows"`
}

func normalizeAppDBColumns(cols []AppDBColumn) []AppDBColumn {
	out := make([]AppDBColumn, 0, len(cols))
	seen := make(map[string]bool, len(cols))
	for _, c := range cols {
		name := strings.TrimSpace(c.Name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		typ := strings.TrimSpace(c.Type)
		if !appDBColumnTypes[typ] {
			typ = "string"
		}
		out = append(out, AppDBColumn{Name: name, Type: typ})
		if len(out) >= appDBColumnLimit {
			break
		}
	}
	return out
}

// inferAppDBColumnType maps one JSON-decoded value to an app-db column type.
// Numbers are float64 after a JSON round-trip; arrays are []any; everything else
// (including ISO date strings, which are indistinguishable from text here)
// renders and synthesizes fine as "string".
func inferAppDBColumnType(v any) string {
	switch v.(type) {
	case float64, float32, int, int32, int64:
		return "number"
	case []any, []string:
		return "string[]"
	default:
		return "string"
	}
}

// inferAppDBColumns derives a column set from sample rows for a table an authored
// tool upserts into WITHOUT a prior define — the "just works" path so an agent
// can CREATE its own data (a Briefings log, a daily digest) instead of failing
// because the schema was never declared. Column order is deterministic: "id"
// first when present, then the remaining keys alphabetically. A column's type is
// the type of its first non-nil value across the rows, defaulting to "string".
func inferAppDBColumns(rows []map[string]any) []AppDBColumn {
	inferred := map[string]string{}
	var order []string
	for _, row := range rows {
		for name := range row {
			n := strings.TrimSpace(name)
			if n == "" {
				continue
			}
			if _, seen := inferred[n]; !seen {
				order = append(order, n)
				inferred[n] = ""
			}
		}
		for name, v := range row {
			n := strings.TrimSpace(name)
			if n == "" || v == nil {
				continue
			}
			if inferred[n] == "" {
				inferred[n] = inferAppDBColumnType(v)
			}
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		if order[i] == "id" {
			return order[j] != "id"
		}
		if order[j] == "id" {
			return false
		}
		return order[i] < order[j]
	})
	cols := make([]AppDBColumn, 0, len(order))
	for _, n := range order {
		typ := inferred[n]
		if typ == "" {
			typ = "string"
		}
		cols = append(cols, AppDBColumn{Name: n, Type: typ})
	}
	return normalizeAppDBColumns(cols)
}

func validateAppDBTableName(table string) (string, error) {
	table = strings.TrimSpace(table)
	if table == "" {
		return "", newCustomAppCallerError("app db: table name is required")
	}
	if len(table) > 64 {
		return "", newCustomAppCallerError("app db: table name too long")
	}
	return table, nil
}

// readAppDBLocked loads the app's db.json. A missing file is not an error: a
// fresh app simply has no tables yet. Callers must hold s.mu.
func (s *customAppStore) readAppDBLocked(id string) (appDB, error) {
	raw, err := os.ReadFile(filepath.Join(s.appDir(id), customAppDBFile))
	if err != nil {
		if os.IsNotExist(err) {
			return appDB{Tables: map[string]appDBTableBody{}}, nil
		}
		return appDB{}, fmt.Errorf("app db: read: %w", err)
	}
	var db appDB
	if err := json.Unmarshal(raw, &db); err != nil {
		return appDB{}, fmt.Errorf("app db: decode: %w", err)
	}
	if db.Tables == nil {
		db.Tables = map[string]appDBTableBody{}
	}
	return db, nil
}

// writeAppDBLocked persists the app's db.json atomically. Callers must hold s.mu.
func (s *customAppStore) writeAppDBLocked(id string, db appDB) error {
	if db.Tables == nil {
		db.Tables = map[string]appDBTableBody{}
	}
	body, err := json.MarshalIndent(db, "", "  ")
	if err != nil {
		return fmt.Errorf("app db: encode: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(s.appDir(id), customAppDBFile), body, 0o600); err != nil {
		return fmt.Errorf("app db: write: %w", err)
	}
	return nil
}

// ensureAppExistsLocked confirms the app's manifest is present, mapping a missing
// app to a caller error (404) rather than silently creating a db for a ghost id.
func (s *customAppStore) ensureAppExistsLocked(id string) error {
	if _, err := s.readManifestLocked(id); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return newCustomAppCallerError("app: %s not found", id)
		}
		return err
	}
	return nil
}

func tableToOut(name string, body appDBTableBody) AppDBTable {
	cols := body.Columns
	if cols == nil {
		cols = []AppDBColumn{}
	}
	rows := body.Rows
	if rows == nil {
		rows = []map[string]any{}
	}
	return AppDBTable{Name: name, Columns: cols, Rows: rows}
}

// AppDBTables returns every table for the app, sorted by name for a stable
// render order. An app with no db.json returns an empty slice, not an error.
func (s *customAppStore) AppDBTables(id string) ([]AppDBTable, error) {
	if err := validateCustomAppID(id); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureAppExistsLocked(id); err != nil {
		return nil, err
	}
	db, err := s.readAppDBLocked(id)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(db.Tables))
	for name := range db.Tables {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]AppDBTable, 0, len(names))
	for _, name := range names {
		out = append(out, tableToOut(name, db.Tables[name]))
	}
	return out, nil
}

// DefineAppDBTable creates a table or replaces its column set, preserving any
// existing rows. It is idempotent: defining the same shape twice is a no-op
// beyond re-normalizing columns.
func (s *customAppStore) DefineAppDBTable(id, table string, columns []AppDBColumn) (AppDBTable, error) {
	if err := validateCustomAppID(id); err != nil {
		return AppDBTable{}, err
	}
	table, err := validateAppDBTableName(table)
	if err != nil {
		return AppDBTable{}, err
	}
	cols := normalizeAppDBColumns(columns)
	if len(cols) == 0 {
		return AppDBTable{}, newCustomAppCallerError("app db: define requires at least one column")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureAppExistsLocked(id); err != nil {
		return AppDBTable{}, err
	}
	db, err := s.readAppDBLocked(id)
	if err != nil {
		return AppDBTable{}, err
	}
	existing, ok := db.Tables[table]
	if !ok && len(db.Tables) >= appDBTableLimit {
		return AppDBTable{}, newCustomAppCallerError("app db: too many tables (max %d)", appDBTableLimit)
	}
	existing.Columns = cols
	if existing.Rows == nil {
		existing.Rows = []map[string]any{}
	}
	db.Tables[table] = existing
	if err := s.writeAppDBLocked(id, db); err != nil {
		return AppDBTable{}, err
	}
	return tableToOut(table, existing), nil
}

// UpsertAppDBRows appends rows to a table, or — when key names a column — replaces
// the row whose key value matches an incoming row (primary-key dedup). The table
// must have been defined first. Row growth is capped so a keyless app that
// re-upserts on every mount cannot grow the store without bound.
func (s *customAppStore) UpsertAppDBRows(id, table string, rows []map[string]any, key string) (AppDBTable, error) {
	if err := validateCustomAppID(id); err != nil {
		return AppDBTable{}, err
	}
	table, err := validateAppDBTableName(table)
	if err != nil {
		return AppDBTable{}, err
	}
	key = strings.TrimSpace(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureAppExistsLocked(id); err != nil {
		return AppDBTable{}, err
	}
	db, err := s.readAppDBLocked(id)
	if err != nil {
		return AppDBTable{}, err
	}
	table = resolveTableBodyName(db, table)
	body, ok := db.Tables[table]
	if !ok {
		// Auto-define from the incoming rows so an authored tool can CREATE a new
		// table (a Briefings log, a digest) without a separate define step — the
		// "save this" the operator described just works, instead of a 400 the
		// agent has no capability to recover from (2026-08-18 output-quality pass).
		if len(db.Tables) >= appDBTableLimit {
			return AppDBTable{}, newCustomAppCallerError("app db: too many tables (max %d)", appDBTableLimit)
		}
		cols := inferAppDBColumns(rows)
		if len(cols) == 0 {
			return AppDBTable{}, newCustomAppCallerError("app db: table %q is not defined and no columns could be inferred from the rows", table)
		}
		body = appDBTableBody{Columns: cols, Rows: []map[string]any{}}
		db.Tables[table] = body
	}
	if body.Rows == nil {
		body.Rows = []map[string]any{}
	}
	// The key must name a DEFINED column: a misspelled key would render every
	// row's key value as "" and collapse unrelated rows into one another.
	if key != "" && !appDBHasColumn(body.Columns, key) {
		return AppDBTable{}, newCustomAppCallerError("app db: key column %q is not defined in %q", key, table)
	}
	// Index existing rows by key value for O(1) dedup when a key is given. Rows
	// without a usable key value (legacy keyless appends) are never dedup
	// targets — they must not all collapse onto the "" key.
	keyIndex := map[string]int{}
	if key != "" {
		for i, row := range body.Rows {
			if kv := appDBKeyValue(row[key]); kv != "" {
				keyIndex[kv] = i
			}
		}
	}
	for i, incoming := range rows {
		if incoming == nil {
			continue
		}
		if key != "" {
			kv := appDBKeyValue(incoming[key])
			if kv == "" {
				// A missing/empty key value cannot dedup; silently appending (or
				// worse, overwriting another empty-keyed row) hides an app bug.
				return AppDBTable{}, newCustomAppCallerError("app db: row %d is missing a value for key column %q", i, key)
			}
			if idx, found := keyIndex[kv]; found {
				body.Rows[idx] = incoming
				continue
			}
			keyIndex[kv] = len(body.Rows)
		}
		if len(body.Rows) >= appDBRowLimit {
			return AppDBTable{}, newCustomAppCallerError("app db: too many rows in %q (max %d)", table, appDBRowLimit)
		}
		body.Rows = append(body.Rows, incoming)
	}
	db.Tables[table] = body
	if err := s.writeAppDBLocked(id, db); err != nil {
		return AppDBTable{}, err
	}
	return tableToOut(table, body), nil
}

// resolveTableBodyName returns the stored table name matching `want`
// case-insensitively, or `want` unchanged when no such table exists (so a
// genuine "not defined" error still fires). The app UI and an authored tool are
// written by different model calls that do not reliably agree on casing
// ("Incidents" vs "incidents"), so reads/writes tolerate it (2026-08-18 loop
// audit — a tool queried a lowercase name and silently found zero rows).
func resolveTableBodyName(db appDB, want string) string {
	if _, ok := db.Tables[want]; ok {
		return want
	}
	for name := range db.Tables {
		if strings.EqualFold(name, want) {
			return name
		}
	}
	return want
}

// QueryAppDBTable returns a single table by name.
func (s *customAppStore) QueryAppDBTable(id, table string) (AppDBTable, error) {
	if err := validateCustomAppID(id); err != nil {
		return AppDBTable{}, err
	}
	table, err := validateAppDBTableName(table)
	if err != nil {
		return AppDBTable{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureAppExistsLocked(id); err != nil {
		return AppDBTable{}, err
	}
	db, err := s.readAppDBLocked(id)
	if err != nil {
		return AppDBTable{}, err
	}
	table = resolveTableBodyName(db, table)
	body, ok := db.Tables[table]
	if !ok {
		return AppDBTable{}, newCustomAppCallerError("app db: table %q is not defined", table)
	}
	return tableToOut(table, body), nil
}

// ClearAppDBTable empties a table's rows, keeping its column definition.
func (s *customAppStore) ClearAppDBTable(id, table string) (AppDBTable, error) {
	if err := validateCustomAppID(id); err != nil {
		return AppDBTable{}, err
	}
	table, err := validateAppDBTableName(table)
	if err != nil {
		return AppDBTable{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureAppExistsLocked(id); err != nil {
		return AppDBTable{}, err
	}
	db, err := s.readAppDBLocked(id)
	if err != nil {
		return AppDBTable{}, err
	}
	table = resolveTableBodyName(db, table)
	body, ok := db.Tables[table]
	if !ok {
		return AppDBTable{}, newCustomAppCallerError("app db: table %q is not defined", table)
	}
	body.Rows = []map[string]any{}
	db.Tables[table] = body
	if err := s.writeAppDBLocked(id, db); err != nil {
		return AppDBTable{}, err
	}
	return tableToOut(table, body), nil
}

// appDBHasColumn reports whether name is a defined column of the table.
func appDBHasColumn(cols []AppDBColumn, name string) bool {
	for _, c := range cols {
		if c.Name == name {
			return true
		}
	}
	return false
}

// appDBKeyValue renders a row's key-column value to a stable string for dedup.
// Numbers, strings, and bools compare by their JSON text; nil is the empty key.
func appDBKeyValue(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(raw)
	}
}
