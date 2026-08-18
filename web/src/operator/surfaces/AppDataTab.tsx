// AppDataTab — the Data tab: the app's real, persisted BACKING DATABASE.
//
// Every app has a small typed store of its own (per app, server-side). The app
// derives its model ONCE from the source it reads, persists it with the bridge
// `db.*` API (defineTable + upsert), and renders from it — see "The app's
// database" in the app-scaffold AI_RULES. This tab is a DETERMINISTIC, direct
// read of that store: GET /apps/{id}/db → the tables the app itself wrote. No AI
// reconstruction, no re-fetch of the source — what the app persisted is what
// shows here, so the two never drift.

import { Fragment, useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";

import { get } from "../../api/client";
import { EmptyState } from "../components/EmptyState";
import { Eyebrow } from "../components/primitives";

interface AppDataTabProps {
  appId: string;
}

interface ModelColumn {
  name: string;
  type: string;
}
interface ModelTable {
  name: string;
  columns: ModelColumn[];
  rows: Record<string, unknown>[];
}

// Parse the broker's GET /apps/{id}/db payload into clean tables. Defensive: the
// wire shape is trusted (our own broker), but tolerate missing fields so a
// half-written table never crashes the tab.
function parseTables(raw: unknown): ModelTable[] {
  const tables = (raw as { tables?: unknown })?.tables;
  if (!Array.isArray(tables)) return [];
  return tables.map((t) => {
    const tt = (t ?? {}) as {
      name?: unknown;
      columns?: unknown;
      rows?: unknown;
    };
    const columns = Array.isArray(tt.columns)
      ? tt.columns
          .map((c) => {
            const cc = (c ?? {}) as { name?: unknown; type?: unknown };
            return {
              name: String(cc.name ?? "").trim(),
              type: String(cc.type ?? "string").trim() || "string",
            };
          })
          .filter((c) => c.name)
      : [];
    // Keep only plain objects: a null or array entry would crash the cell
    // lookup (row[c.name]) at render time.
    const rows = Array.isArray(tt.rows)
      ? tt.rows.filter(
          (r): r is Record<string, unknown> =>
            !!r && typeof r === "object" && !Array.isArray(r),
        )
      : [];
    return {
      name: String(tt.name ?? "Table").trim() || "Table",
      columns,
      rows,
    };
  });
}

export function AppDataTab({ appId }: AppDataTabProps) {
  const dbQuery = useQuery({
    queryKey: ["operator-app-db", appId],
    // The app writes to its DB through the bridge in a different component
    // tree, so nothing invalidates this key. It is a cheap local read: always
    // refetch on mount so the tab never shows a stale snapshot.
    refetchOnMount: "always",
    queryFn: async (): Promise<ModelTable[]> => {
      const res = await get<{ tables?: unknown }>(
        `/apps/${encodeURIComponent(appId)}/db`,
      );
      return parseTables(res);
    },
  });

  if (dbQuery.isLoading) {
    // Table-shaped skeleton, not a 320px void: the wait previews the shape
    // of what loads (2026-08-16 delight audit).
    return (
      <div className="opr-tool-scoped" role="status" aria-label="Loading data">
        <div className="opr-skeleton opr-skel-row" />
        <div className="opr-skeleton opr-skel-row" style={{ marginTop: 8 }} />
        <div className="opr-skeleton opr-skel-row" style={{ marginTop: 8 }} />
      </div>
    );
  }

  if (dbQuery.isError) {
    return (
      <EmptyState
        glyph="▦"
        title="Could not read this agent’s data"
        hint="The workspace could not load this agent’s database right now. Try again in a moment."
      />
    );
  }

  const tables = dbQuery.data ?? [];
  if (tables.length === 0) {
    return (
      <EmptyState
        glyph="▦"
        portraitSlug={appId}
        title="No data yet"
        hint="Nothing saved yet. After its first run, everything this agent records lands here as tables you own: browse every row, export any table as CSV or JSON. No BI ticket required."
      />
    );
  }

  return (
    <div className="opr-tool-scoped opr-app-data">
      <div className="opr-data-intro">
        <Eyebrow>This agent’s database</Eyebrow>
        <p className="opr-scoped-note">
          The tables this agent derived and saved, read straight from its own
          database. Nothing reconstructed. Every table exports as CSV or JSON:
          your data, no export ticket.
        </p>
      </div>
      {tables.map((t, i) => (
        // Name+index key: parseTables falls back to "Table" for a half-written
        // table, so bare names can collide and misapply reconciliation.
        <ModelTableView key={`${t.name}-${i}`} table={t} />
      ))}
    </div>
  );
}

// ── Making stored values LEGIBLE ────────────────────────────────────────────
//
// Apps persist real shapes: JSON-encoded arrays of findings, ISO timestamps,
// long strings. Rendering those as flat truncated text made the tab useless —
// the 2026-08-15 QA verdict was "the data there does not seem usable". The
// rules here turn storage back into information without inventing anything:
//   - JSON-in-string cells parse and render structurally (arrays of objects
//     become nested mini-tables on expand; empty arrays read "none").
//   - Dates humanize ("Aug 15, 9:59 AM"), full ISO on hover.
//   - Every row expands to a full detail view, so truncation never hides data.
//   - Each table exports as CSV or JSON — the operator owns this data.

type Parsed =
  | { kind: "empty" }
  | { kind: "scalar"; text: string }
  | { kind: "number"; text: string; value: number }
  | { kind: "bool"; value: boolean }
  | { kind: "date"; text: string; full: string }
  | { kind: "list"; items: unknown[] }
  | { kind: "record"; value: Record<string, unknown> };

const ISO_DATE_RE = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}/;
const BARE_DATE_RE = /^(\d{4})-(\d{2})-(\d{2})$/;

function humanizeDate(iso: string): string {
  // A bare date ("2026-09-16") has no time and no timezone. Date.parse reads
  // it as UTC midnight, so toLocaleString would shift it into the previous
  // local day ("Sep 15, 5:00 PM"). Build it from parts as a local date and
  // show no time-of-day the data never had.
  const bare = BARE_DATE_RE.exec(iso);
  if (bare) {
    const d = new Date(Number(bare[1]), Number(bare[2]) - 1, Number(bare[3]));
    return d.toLocaleDateString(undefined, {
      month: "short",
      day: "numeric",
      year: "numeric",
    });
  }
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "numeric",
    minute: "2-digit",
  });
}

function formatNumber(n: number): string {
  // Group digits so "$8400" reads as "8,400"; keep up to 2 decimals without
  // forcing trailing zeros on whole numbers.
  return n.toLocaleString(undefined, { maximumFractionDigits: 2 });
}

function parseValue(v: unknown, colType: string): Parsed {
  if (v == null || v === "") return { kind: "empty" };
  if (typeof v === "boolean") return { kind: "bool", value: v };
  if (typeof v === "number") {
    return Number.isFinite(v)
      ? { kind: "number", text: formatNumber(v), value: v }
      : { kind: "scalar", text: String(v) };
  }
  if (typeof v === "string") {
    const t = v.trim();
    if (
      (colType === "date" || ISO_DATE_RE.test(t)) &&
      !Number.isNaN(Date.parse(t))
    ) {
      return { kind: "date", text: humanizeDate(t), full: t };
    }
    // A number column whose value arrived as a string ("8400") still renders
    // as a grouped number — but only when the column is typed number, so a
    // ZIP code or SKU in a string column is never mangled into "12,345".
    if (colType === "number" && t !== "" && !Number.isNaN(Number(t))) {
      return {
        kind: "number",
        text: formatNumber(Number(t)),
        value: Number(t),
      };
    }
    if (t.startsWith("[") || t.startsWith("{")) {
      try {
        return parseValue(JSON.parse(t), colType);
      } catch {
        // Not JSON after all — fall through to scalar.
      }
    }
    return { kind: "scalar", text: t };
  }
  if (Array.isArray(v)) {
    return v.length === 0 ? { kind: "empty" } : { kind: "list", items: v };
  }
  if (typeof v === "object") {
    return { kind: "record", value: v as Record<string, unknown> };
  }
  return { kind: "scalar", text: String(v) };
}

// ── Filter + sort: pure helpers, unit-tested ────────────────────────────────

/** Flatten any cell value to a lowercased searchable string (nested JSON too). */
function cellSearchText(v: unknown): string {
  if (v == null) return "";
  if (typeof v === "object") {
    try {
      return JSON.stringify(v).toLowerCase();
    } catch {
      return "";
    }
  }
  return String(v).toLowerCase();
}

/** Case-insensitive substring match across every column of a row. */
export function rowMatchesQuery(
  row: Record<string, unknown>,
  columns: ModelColumn[],
  query: string,
): boolean {
  const q = query.trim().toLowerCase();
  if (q === "") return true;
  return columns.some((c) => cellSearchText(row[c.name]).includes(q));
}

/**
 * Type-aware comparator for a column. Numbers compare numerically ("10" after
 * "9"), dates by timestamp, everything else by locale string. Empty values sort
 * last regardless of direction so blanks never crowd the top.
 */
export function compareByColumn(
  a: Record<string, unknown>,
  b: Record<string, unknown>,
  col: ModelColumn,
  dir: "asc" | "desc",
): number {
  const av = a[col.name];
  const bv = b[col.name];
  const aEmpty = av == null || av === "";
  const bEmpty = bv == null || bv === "";
  if (aEmpty && bEmpty) return 0;
  if (aEmpty) return 1;
  if (bEmpty) return -1;
  const sign = dir === "asc" ? 1 : -1;
  if (col.type === "number") {
    return (Number(av) - Number(bv)) * sign;
  }
  if (col.type === "date") {
    return (Date.parse(String(av)) - Date.parse(String(bv))) * sign;
  }
  return String(av).localeCompare(String(bv)) * sign;
}

/** Short inline summary for a cell; the row expansion carries the detail. */
function CellSummary({ parsed }: { parsed: Parsed }) {
  switch (parsed.kind) {
    case "empty":
      return <span className="opr-data-none">none</span>;
    case "number":
      return <span className="opr-data-num">{parsed.text}</span>;
    case "bool":
      return (
        <span className={`opr-data-bool${parsed.value ? " is-yes" : " is-no"}`}>
          {parsed.value ? "Yes" : "No"}
        </span>
      );
    case "date":
      return <span title={parsed.full}>{parsed.text}</span>;
    case "list": {
      const scalars = parsed.items.every((x) => typeof x !== "object");
      if (scalars) {
        const joined = parsed.items.map((x) => String(x)).join(", ");
        return (
          <span title={joined}>
            {joined.length > 60 ? `${joined.slice(0, 60)}…` : joined}
          </span>
        );
      }
      return (
        <span className="opr-data-chip">
          {parsed.items.length} {parsed.items.length === 1 ? "item" : "items"}
        </span>
      );
    }
    case "record":
      return <span className="opr-data-chip">details</span>;
    default:
      return (
        <span title={parsed.text.length > 60 ? parsed.text : undefined}>
          {parsed.text.length > 60
            ? `${parsed.text.slice(0, 60)}…`
            : parsed.text}
        </span>
      );
  }
}

/** Full-width structural rendering used inside a row's expansion. */
function ValueDetail({ parsed }: { parsed: Parsed }) {
  switch (parsed.kind) {
    case "empty":
      return <span className="opr-data-none">none</span>;
    case "number":
      return <span className="opr-data-num">{parsed.text}</span>;
    case "bool":
      return (
        <span className={`opr-data-bool${parsed.value ? " is-yes" : " is-no"}`}>
          {parsed.value ? "Yes" : "No"}
        </span>
      );
    case "date":
      return (
        <span>
          {parsed.text} <span className="opr-data-none">({parsed.full})</span>
        </span>
      );
    case "list": {
      const objects = parsed.items.filter(
        (x): x is Record<string, unknown> =>
          !!x && typeof x === "object" && !Array.isArray(x),
      );
      if (objects.length === parsed.items.length && objects.length > 0) {
        // Array of records → a readable nested table over the union of keys.
        const keys: string[] = [];
        for (const o of objects) {
          for (const k of Object.keys(o)) if (!keys.includes(k)) keys.push(k);
        }
        return (
          <table className="opr-data-table opr-data-nested">
            <thead>
              <tr>
                {keys.map((k) => (
                  <th key={k}>
                    <span className="opr-data-col-name">{k}</span>
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {objects.map((o, i) => (
                <tr key={`${String(o[keys[0]] ?? "")}-${i}`}>
                  {keys.map((k) => (
                    <td key={k}>
                      <CellSummary parsed={parseValue(o[k], "string")} />
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        );
      }
      return <span>{parsed.items.map((x) => String(x)).join(", ")}</span>;
    }
    case "record":
      return (
        <dl className="opr-data-kv">
          {Object.entries(parsed.value).map(([k, v]) => (
            <div className="opr-data-kv-row" key={k}>
              <dt>{k}</dt>
              <dd>
                <ValueDetail parsed={parseValue(v, "string")} />
              </dd>
            </div>
          ))}
        </dl>
      );
    default:
      return <span className="opr-data-full-text">{parsed.text}</span>;
  }
}

// ── Export: the operator owns this data ─────────────────────────────────────

function downloadBlob(filename: string, mime: string, content: string) {
  const url = URL.createObjectURL(new Blob([content], { type: mime }));
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  a.click();
  URL.revokeObjectURL(url);
}

function csvEscape(v: unknown): string {
  // Objects/arrays serialize to JSON (matching the JSON export), never the
  // useless "[object Object]" String() would produce.
  const s =
    v == null ? "" : typeof v === "object" ? JSON.stringify(v) : String(v);
  return /[",\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
}

function exportTable(table: ModelTable, format: "csv" | "json") {
  if (format === "json") {
    downloadBlob(
      `${table.name}.json`,
      "application/json",
      JSON.stringify(table.rows, null, 2),
    );
    return;
  }
  const cols = table.columns.map((c) => c.name);
  const lines = [
    cols.map(csvEscape).join(","),
    ...table.rows.map((r) => cols.map((c) => csvEscape(r[c])).join(",")),
  ];
  downloadBlob(`${table.name}.csv`, "text/csv", lines.join("\n"));
}

const PAGE_SIZE = 25;

interface SortState {
  col: string;
  dir: "asc" | "desc";
}

function ModelTableView({ table }: { table: ModelTable }) {
  // Expansion is keyed on the row OBJECT, not an index: filtering, sorting, and
  // paging all reorder the visible rows, so an index would open the wrong row.
  const [openRow, setOpenRow] = useState<Record<string, unknown> | null>(null);
  const [query, setQuery] = useState("");
  const [sort, setSort] = useState<SortState | null>(null);
  const [page, setPage] = useState(0);

  const filtered = useMemo(
    () => table.rows.filter((r) => rowMatchesQuery(r, table.columns, query)),
    [table.rows, table.columns, query],
  );

  const sorted = useMemo(() => {
    if (!sort) return filtered;
    const col = table.columns.find((c) => c.name === sort.col);
    if (!col) return filtered;
    // Copy before sort: never mutate the query cache's row array in place.
    return [...filtered].sort((a, b) => compareByColumn(a, b, col, sort.dir));
  }, [filtered, sort, table.columns]);

  // Clamp the page whenever the result set shrinks (a query narrowing past the
  // current page must not leave the operator staring at an empty slice).
  const pageCount = Math.max(1, Math.ceil(sorted.length / PAGE_SIZE));
  const safePage = Math.min(page, pageCount - 1);
  const start = safePage * PAGE_SIZE;
  const paged = sorted.slice(start, start + PAGE_SIZE);

  const total = table.rows.length;
  const filteredOut = query.trim() !== "" && filtered.length !== total;

  function toggleSort(colName: string) {
    setPage(0);
    setSort((prev) =>
      prev?.col === colName
        ? { col: colName, dir: prev.dir === "asc" ? "desc" : "asc" }
        : { col: colName, dir: "asc" },
    );
  }

  return (
    <div className="opr-data-block">
      <div className="opr-data-block-head">
        {table.name}
        <span className="opr-data-block-sub">
          {filteredOut
            ? `${filtered.length} of ${total} rows`
            : `${total} ${total === 1 ? "row" : "rows"}`}
        </span>
        {total > 0 ? (
          <span className="opr-data-export">
            <button
              type="button"
              className="opr-btn opr-btn-sm"
              onClick={() => exportTable(table, "csv")}
            >
              CSV
            </button>
            <button
              type="button"
              className="opr-btn opr-btn-sm"
              onClick={() => exportTable(table, "json")}
            >
              JSON
            </button>
          </span>
        ) : null}
      </div>
      {total === 0 ? (
        <div className="opr-data-empty">
          Defined, no rows yet — the agent has declared this table but not
          written to it.
        </div>
      ) : (
        <>
          {total > 8 ? (
            <div className="opr-data-toolbar">
              <input
                type="search"
                className="opr-data-search"
                placeholder={`Search ${table.name.toLowerCase()}…`}
                aria-label={`Search ${table.name}`}
                value={query}
                onChange={(e) => {
                  setQuery(e.target.value);
                  setPage(0);
                }}
              />
            </div>
          ) : null}
          {filtered.length === 0 ? (
            <div className="opr-data-empty">
              No rows match “{query.trim()}”.
            </div>
          ) : (
            <table className="opr-data-table">
              <thead>
                <tr>
                  {table.columns.map((c) => {
                    const active = sort?.col === c.name;
                    const ariaSort = active
                      ? sort?.dir === "asc"
                        ? "ascending"
                        : "descending"
                      : "none";
                    return (
                      <th
                        key={c.name}
                        aria-sort={ariaSort}
                        className="opr-data-th-sortable"
                      >
                        <button
                          type="button"
                          className="opr-data-sort-btn"
                          onClick={() => toggleSort(c.name)}
                        >
                          <span className="opr-data-col-name">{c.name}</span>
                          <span className="opr-data-col-type">{c.type}</span>
                          <span
                            className="opr-data-sort-caret"
                            aria-hidden={true}
                          >
                            {active ? (sort?.dir === "asc" ? "▲" : "▼") : "↕"}
                          </span>
                        </button>
                      </th>
                    );
                  })}
                </tr>
              </thead>
              <tbody>
                {paged.map((row, i) => {
                  const open = openRow === row;
                  return (
                    <Fragment key={`row-${start + i}`}>
                      <tr
                        className={`opr-dt-row${open ? " is-open" : ""}`}
                        onClick={() => setOpenRow(open ? null : row)}
                      >
                        {table.columns.map((c) => (
                          <td key={c.name}>
                            <CellSummary
                              parsed={parseValue(row[c.name], c.type)}
                            />
                          </td>
                        ))}
                      </tr>
                      {open ? (
                        <tr className="opr-data-detail-row">
                          <td colSpan={table.columns.length}>
                            <dl className="opr-data-kv">
                              {table.columns.map((c) => (
                                <div className="opr-data-kv-row" key={c.name}>
                                  <dt>{c.name}</dt>
                                  <dd>
                                    <ValueDetail
                                      parsed={parseValue(row[c.name], c.type)}
                                    />
                                  </dd>
                                </div>
                              ))}
                            </dl>
                          </td>
                        </tr>
                      ) : null}
                    </Fragment>
                  );
                })}
              </tbody>
            </table>
          )}
          {sorted.length > PAGE_SIZE ? (
            <div className="opr-data-pager">
              <button
                type="button"
                className="opr-btn opr-btn-sm"
                disabled={safePage === 0}
                onClick={() => setPage(safePage - 1)}
              >
                Prev
              </button>
              <span className="opr-data-pager-label">
                Showing {start + 1}–{Math.min(start + PAGE_SIZE, sorted.length)}{" "}
                of {sorted.length}
              </span>
              <button
                type="button"
                className="opr-btn opr-btn-sm"
                disabled={safePage >= pageCount - 1}
                onClick={() => setPage(safePage + 1)}
              >
                Next
              </button>
            </div>
          ) : null}
        </>
      )}
    </div>
  );
}
