package team

// broker_apps.go owns the HTTP surface for agent-generated internal tools
// ("Apps"). Routes are reached only via the /api proxy (ServeWebUI strips /api
// and forwards to this mux), so they never collide with the SPA's client-side
// /apps/<id> route — the browser navigation hits the SPA fallback, the data
// fetch hits these handlers.
//
//	GET    /apps          -> { apps: [...] }            (sidebar listing)
//	POST   /apps          -> { app }                    (App Builder register/update)
//	GET    /apps/{id}      -> { app, html }             (render in sandboxed iframe)
//	PATCH  /apps/{id}      -> { app }                   (rename an app)
//	DELETE /apps/{id}      -> { ok: true }              (remove an app)

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// appStore lazily constructs the custom-app store on first use so we avoid
// constructor surgery on NewBroker. Safe for concurrent callers via sync.Once.
func (b *Broker) appStore() *customAppStore {
	b.customAppOnce.Do(func() {
		b.customApps = newCustomAppStore(CustomAppsRootDir())
	})
	return b.customApps
}

func (b *Broker) handleApps(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		apps, err := b.appStore().List()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"apps": apps})
	case http.MethodPost:
		var body CustomAppWriteRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		// Resolve the writer the same way rich artifacts do: prefer the
		// authenticated agent slug, fall back to the human session identity.
		actor, status, err := richArtifactAuthenticatedSlug(r, body.Actor, "actor")
		if err != nil {
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		// Only the App Builder (the lone agent with register_app) or a human
		// session may write app bytes. A random agent holding the broker token
		// must not register apps directly and bypass the build path.
		if !b.appWriterAllowed(r, actor) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "only the App Builder may register apps"})
			return
		}
		body.Actor = actor
		app, err := b.appStore().Save(body, time.Now())
		if err != nil {
			writeAppError(w, err)
			return
		}
		// The app's deterministic workflow is a PART of the app, not an
		// afterthought — so figure it out at publish time: precompile + freeze it
		// off the request path so the Workflow tab opens already-compiled instead
		// of compiling on demand while the operator watches. Fail-safe; the
		// on-demand compile remains as the fallback.
		go b.precompileAppWorkflowAsync(app.ID)
		// First human build -> mint the starter routine server-side (the FE
		// chat used to own this and lost it whenever it unmounted mid-build).
		go b.mintStarterRoutineForFirstBuild(app)
		// Deterministic post-publish sanity note: if the saved page is still
		// the scaffold placeholder or implausibly small, say so once in the
		// edit channel. Advisory only — never blocks, reopens, or retries.
		go b.advisePublishOddities(app)
		// A republish rewrites the source and may change the dependency set, so
		// any running live-preview server is now stale. Stop it and pre-warm a
		// fresh one in the background: the new deps (e.g. the refine stack)
		// install off the request path, so the Live tab is ready instead of a
		// blank cold boot when the human opens it.
		mgr := b.appDevManager()
		mgr.Stop(app.ID)
		go func(id string) { _, _ = mgr.Ensure(id) }(app.ID)
		writeJSON(w, http.StatusOK, map[string]any{"app": app})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (b *Broker) handleAppByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/apps/"), "/")
	parts := strings.Split(rest, "/")
	id := parts[0]
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}
	if err := validateCustomAppID(id); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	switch sub {
	case "":
		b.handleAppRoot(w, r, id)
	case "versions":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// /apps/{id}/versions/{n} reads one retained build for non-destructive
		// preview; /apps/{id}/versions lists them.
		if len(parts) > 2 {
			b.handleAppVersion(w, r, id, parts[2])
			return
		}
		versions, err := b.appStore().ListVersions(id)
		if err != nil {
			writeAppError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"versions": versions})
	case "rollback":
		b.handleAppRollback(w, r, id)
	case "edit-session":
		b.handleAppEditSession(w, r, id)
	case "improve":
		b.handleAppImprove(w, r, id)
	case "dev":
		b.handleAppDev(w, r, id, parts)
	case "activity":
		b.handleAppActivity(w, r, id)
	case "db":
		b.handleAppDB(w, r, id)
	case "knowledge":
		b.handleAppKnowledge(w, r, id)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// handleAppActivity streams the app's live build/edit activity (the App
// Builder's thinking + tool calls) as SSE, scoped to THIS app. It resolves the
// app's backing app-builder run from its EditChannel and delegates to the shared
// agent-stream writer, so the operator surface subscribes by app id alone and
// never threads a task id. An app whose run has not started yet streams an empty
// (replay-end + heartbeat) connection rather than erroring, so the FE can attach
// the feed the instant a build begins.
//
//	GET /apps/{id}/activity  -> text/event-stream of the app's build/edit run
func (b *Broker) handleAppActivity(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	app, _, err := b.appStore().Get(id)
	if err != nil {
		writeAppError(w, err)
		return
	}
	// Scope by the backing run's task id. When no run exists yet, scope by the
	// app id instead: no task event carries that tag, so the client gets an
	// empty stream that fills once the run starts — never the global app-builder
	// feed (which would leak other apps' activity).
	scope := b.appBuilderRunTaskID(app.EditChannel)
	if scope == "" {
		scope = id
	}
	b.streamAgentTaskSSE(w, r, appBuilderSlug, scope)
}

// appBuilderRunTaskID resolves an app's persistent edit channel to the id of the
// app-builder task that backs it (the build/improve run whose events feed the
// activity stream). Returns "" when no such task exists yet.
func (b *Broker) appBuilderRunTaskID(editChannel string) string {
	editChannel = strings.TrimSpace(editChannel)
	if editChannel == "" {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.tasks {
		if b.tasks[i].Channel == editChannel && b.tasks[i].Owner == appBuilderSlug {
			return b.tasks[i].ID
		}
	}
	return ""
}

// appDBOpRequest is the POST /apps/{id}/db body: an op selector plus the
// op-specific fields. One endpoint, op-dispatched, so the Bridge speaks a single
// db channel rather than a route per verb.
type appDBOpRequest struct {
	Op      string           `json:"op"`
	Table   string           `json:"table"`
	Columns []AppDBColumn    `json:"columns"`
	Rows    []map[string]any `json:"rows"`
	Key     string           `json:"key"`
}

// handleAppDB is the app's backing DATABASE: named tables, typed columns, rows.
// The app writes its derived model here (via the Bridge) and renders from it; the
// Data tab reads it directly.
//
// AUTH: the whole /apps/ tree is behind requireAuth, so every caller here holds a
// valid broker token or human session. Writes are NOT further gated to
// app-builder/human — unlike app BYTES (register/delete/rollback), which change
// app CODE and so are appWriterAllowed-only. DB rows are app DATA the app writes
// about ITSELF, exactly like the create_task / integration bridge writes, which
// also run under the broker token (the sandboxed app reaches the broker through
// the web proxy carrying that token — it is broker-kind, never a human session,
// so an appWriterAllowed gate would reject the app's own writes). The real
// protections are: authenticated caller + valid app id + server-side bounds
// (table/column/row caps), all enforced below and in custom_app_db.go.
//
//	GET  /apps/{id}/db  -> {tables:[{name,columns,rows}]}
//	POST /apps/{id}/db  -> op-dispatch: define | upsert | query | clear
func (b *Broker) handleAppDB(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodGet:
		tables, err := b.appStore().AppDBTables(id)
		if err != nil {
			writeAppError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"tables": tables})
	case http.MethodPost:
		var req appDBOpRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		store := b.appStore()
		// Throttle WRITE frequency per app: the store caps table/row/column
		// GROWTH, but a tight loop could still keep rewriting db.json under the
		// store mutex. Reads (GET and the "query" op) are unmetered.
		op := strings.TrimSpace(req.Op)
		if op == "define" || op == "upsert" || op == "clear" {
			if retryAfter, limited := b.consumeAppDBWriteBudget(appBudgetKey(id, r)); limited {
				w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
				writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "db write rate limit reached — retry shortly"})
				return
			}
		}
		var (
			table AppDBTable
			opErr error
		)
		switch op {
		case "define":
			table, opErr = store.DefineAppDBTable(id, req.Table, req.Columns)
		case "upsert":
			table, opErr = store.UpsertAppDBRows(id, req.Table, req.Rows, req.Key)
		case "query":
			table, opErr = store.QueryAppDBTable(id, req.Table)
		case "clear":
			table, opErr = store.ClearAppDBTable(id, req.Table)
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown op: want define|upsert|query|clear"})
			return
		}
		if opErr != nil {
			writeAppError(w, opErr)
			return
		}
		// Data changed -> drop the stale wiki cache so the Knowledge tab
		// re-synthesizes against the new rows on next view (2026-08-18
		// output-quality pass). Best-effort: a failed invalidate must not fail
		// the write the operator just made.
		if op == "define" || op == "upsert" || op == "clear" {
			if invErr := store.InvalidateAppKnowledge(id); invErr != nil {
				fmt.Fprintf(os.Stderr, "broker: app knowledge invalidate failed: %v\n", invErr)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"table": table})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// appDevManager lazily constructs the live-preview dev-server manager on first
// use (mirrors appStore). It shares the same custom-app store.
func (b *Broker) appDevManager() *appDevManager {
	b.appDevOnce.Do(func() {
		b.appDev = newAppDevManager(b.appStore())
	})
	return b.appDev
}

// handleAppDev is the CONTROL plane for live previews: ensure/status/stop. The
// preview content itself is served by a per-app reverse proxy on its own
// ephemeral 127.0.0.1 port (see custom_app_dev.go); these endpoints just manage
// its lifecycle and hand the FE the proxy origin to load.
//
//	GET  /apps/{id}/dev         -> ensure running, returns {ready,url,boot_log,error}
//	GET  /apps/{id}/dev/status  -> current status without (re)starting
//	POST /apps/{id}/dev/stop    -> tear down (App Builder / human only)
func (b *Broker) handleAppDev(w http.ResponseWriter, r *http.Request, id string, parts []string) {
	action := ""
	if len(parts) > 2 {
		action = parts[2]
	}
	mgr := b.appDevManager()
	switch {
	case r.Method == http.MethodGet && action == "":
		st, err := mgr.Ensure(id)
		if err != nil {
			writeAppError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	case r.Method == http.MethodGet && action == "status":
		st, ok := mgr.Status(id)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no preview running"})
			return
		}
		writeJSON(w, http.StatusOK, st)
	case r.Method == http.MethodPost && action == "stop":
		actor, status, err := richArtifactAuthenticatedSlug(r, "", "actor")
		if err != nil {
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		if !b.appWriterAllowed(r, actor) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "only a human or the App Builder may stop a preview"})
			return
		}
		mgr.Stop(id)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (b *Broker) handleAppRoot(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodGet:
		app, htmlBody, err := b.appStore().Get(id)
		if err != nil {
			writeAppError(w, err)
			return
		}
		out := map[string]any{"app": app, "html": htmlBody}
		// ?source=1 includes the app's source project — only the App Builder
		// needs it (to edit), so the FE view never asks for it. Alongside the raw
		// source we attach a deterministic capability summary (data model, APIs,
		// office writes, UI) so the agent edits from the app's REAL shape instead
		// of guessing or inventing capabilities it lacks.
		if r.URL.Query().Get("source") == "1" {
			source, err := b.appStore().Source(id)
			if err != nil {
				writeAppError(w, err)
				return
			}
			out["source"] = source
			out["capabilities"] = introspectAppSource(source)
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPatch:
		// Rename: the one metadata mutation on an app. Gated like delete/rollback
		// (a human session or the App Builder) so a random agent holding the
		// broker token cannot rewrite app names. The FE's client-side rename
		// store is a cache of this.
		actor, _, _ := richArtifactAuthenticatedSlug(r, "", "actor")
		if !b.appWriterAllowed(r, actor) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "only a human or the App Builder may rename apps"})
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		app, err := b.appStore().Rename(id, body.Name, actor, time.Now())
		if err != nil {
			writeAppError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"app": app})
	case http.MethodDelete:
		actor, _, _ := richArtifactAuthenticatedSlug(r, "", "actor")
		if !b.appWriterAllowed(r, actor) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "only a human or the App Builder may delete apps"})
			return
		}
		if err := b.appStore().Delete(id); err != nil {
			writeAppError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleAppVersion serves GET /apps/{id}/versions/{n}: one retained build's
// bytes + metadata for non-destructive preview. It NEVER changes the current
// version — restoring is the separate POST /apps/{id}/rollback. The bytes were
// validated at write time and render in the same sandboxed frame as the sealed
// current view, so this adds no new rendering surface.
func (b *Broker) handleAppVersion(w http.ResponseWriter, r *http.Request, id, raw string) {
	n, err := strconv.Atoi(raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid version"})
		return
	}
	ver, htmlBody, err := b.appStore().GetVersion(id, n)
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":   ver.Version,
		"updatedAt": ver.UpdatedAt,
		"updatedBy": ver.UpdatedBy,
		"current":   ver.Current,
		"html":      htmlBody,
	})
}

func (b *Broker) handleAppRollback(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Version int    `json:"version"`
		Actor   string `json:"actor"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	actor, status, err := richArtifactAuthenticatedSlug(r, body.Actor, "actor")
	if err != nil {
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	if !b.appWriterAllowed(r, actor) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "only a human or the App Builder may roll back apps"})
		return
	}
	app, err := b.appStore().Rollback(id, body.Version, actor, time.Now())
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"app": app})
}

// handleAppEditSession opens (or returns) the app's persistent edit thread —
// the per-app "chat to edit" channel:
//
//	POST /apps/{id}/edit-session -> { channel }
//
// Every app should be editable, but an app minted before edit-channel stamping
// (or registered html-only, with no backing task) carries no channel, so the FE
// could not surface Edit for it. This lazily mints one: it creates an App
// Builder "Edit app: <name>" task, and the task-create hook
// (stampAppEditChannelForTaskLocked) stamps the new task-<id> channel onto the
// app SYNCHRONOUSLY, so the bound channel is readable the moment MutateTask
// returns. The App Builder then greets the human in that channel and waits; a
// human post there wakes it through the same task_followup path edits use.
//
// Idempotent: an app already bound to an edit thread returns it untouched, so a
// double-click never spawns a second task. Gated like register/delete — a human
// session or the App Builder only.
func (b *Broker) handleAppEditSession(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	actor, status, err := richArtifactAuthenticatedSlug(r, "", "actor")
	if err != nil {
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	if !b.appWriterAllowed(r, actor) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "only a human or the App Builder may open an edit session"})
		return
	}
	ch, err := b.ensureAppEditChannel(id)
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"channel": ch})
}

// ensureAppEditChannel returns the app's persistent edit channel (`task-<id>`),
// creating the App Builder "Edit app" task that owns it if the app is not bound
// yet. Idempotent: an already-bound app returns its channel without spawning a
// task. Shared by the edit-session and improve handlers.
func (b *Broker) ensureAppEditChannel(id string) (string, error) {
	app, _, err := b.appStore().Get(id)
	if err != nil {
		return "", err
	}
	if ch := strings.TrimSpace(app.EditChannel); ch != "" {
		return ch, nil
	}
	// Ground the edit thread in the app's REAL shape (data model, APIs, writes,
	// UI), derived from its source, so the agent never invents capabilities.
	capsSummary := ""
	if source, serr := b.appStore().Source(id); serr == nil {
		capsSummary = renderAppCapabilities(introspectAppSource(source))
	}
	title, details := appEditSessionBrief(app, capsSummary)
	// MutateTask locks internally; callers hold no broker lock (same pattern as
	// maybeSpawnAppBuilderTaskFromProposal).
	if _, err := b.MutateTask(TaskPostRequest{
		Action:    "create",
		Channel:   "general",
		Title:     title,
		Details:   details,
		Owner:     appBuilderSlug,
		CreatedBy: "human",
		TaskType:  "issue",
	}); err != nil {
		return "", err
	}
	// The create hook stamped the new task-<id> channel onto the app
	// synchronously; re-read the manifest to return the bound channel.
	updated, _, err := b.appStore().Get(id)
	if err != nil {
		return "", err
	}
	ch := strings.TrimSpace(updated.EditChannel)
	if ch == "" {
		return "", fmt.Errorf("edit session created but no channel was bound")
	}
	return ch, nil
}

// handleAppImprove applies a human-requested change to an existing app:
//
//	POST /apps/{id}/improve  { "change": "add a CSV export button" }  -> { channel }
//
// It is the robust, explicit edit path. Rather than minting a NEW "Improve app"
// task (which is created already Running but with no agent turn attending it —
// so the change has nothing to ride and hangs), it ensures the app's settled
// edit-channel and posts the change there as a human message. That post drives
// the proven task_followup wake, which re-engages the App Builder on its OWN
// task (read get_app -> apply -> republish a new version). Completion is observed
// by the app's version bump, not by parsing the agent's narration.
func (b *Broker) handleAppImprove(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	actor, status, err := richArtifactAuthenticatedSlug(r, "", "actor")
	if err != nil {
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	if !b.appWriterAllowed(r, actor) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "only a human or the App Builder may improve an app"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, studioRequestMaxBodyBytes)
	defer r.Body.Close()
	var body struct {
		Change string `json:"change"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	change := strings.TrimSpace(body.Change)
	if change == "" {
		http.Error(w, "change is required", http.StatusBadRequest)
		return
	}
	app, _, err := b.appStore().Get(id)
	if err != nil {
		writeAppError(w, err)
		return
	}
	channel, err := b.ensureAppEditChannel(id)
	if err != nil {
		writeAppError(w, err)
		return
	}
	// Dispatch the App Builder DIRECTLY for this edit — one agent, one job, no
	// CEO/lead hop. The lead route (a human post that wakes the orchestrator) can
	// hit the lead's own turn cap and drop the edit, leaving it silently undone.
	// If no direct enqueuer is wired (e.g. unit tests), fall back to the
	// message-wake path.
	if !b.dispatchAgentTurn(appBuilderSlug, buildAppImprovePrompt(app, change), channel) {
		if _, err := b.PostMessage("human", channel, change, nil, ""); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"channel": channel})
}

// buildAppImprovePrompt is the self-contained turn input for a direct App Builder
// edit: it names the app + change and the exact in-place flow (get_app -> apply
// -> verify -> republish onto the same id), so the builder needs no task brief.
func buildAppImprovePrompt(app CustomApp, change string) string {
	return fmt.Sprintf(
		"A human asked to change the app %q (`%s`). Their request:\n\n%s\n\n"+
			"Apply it IN PLACE: call get_app(%q) to read the current source and "+
			"capabilities, make the change, run the verify gate (`bun run verify`), "+
			"then republish with register_app(app_id=%q). Narrate briefly as you go "+
			"and confirm what changed once it is live. Do NOT create a new app.",
		app.Name, app.ID, change, app.ID, app.ID,
	)
}

// appWriterAllowed reports whether the request may write/delete app bytes:
// the authenticated agent is the App Builder, the caller is an invited human
// session, or the caller is the OWNER — bare token auth with no agent
// identity claimed. Agents always claim their slug via the agent header (and
// any non-app-builder slug is rejected above), so a header-less token caller
// is the human driving the web UI, not an agent sidestepping the gate: the
// header is cooperative, and an agent omitting it gains nothing it could not
// already do with the token it holds. Without the owner lane the operator
// UI's own edit path (submitAppEdit → POST /apps/{id}/improve) 403'd for the
// only human in a single-owner workspace.
func (b *Broker) appWriterAllowed(r *http.Request, actor string) bool {
	if strings.EqualFold(strings.TrimSpace(actor), appBuilderSlug) {
		return true
	}
	a, ok := requestActorFromContext(r.Context())
	if !ok {
		return false
	}
	if a.Kind == requestActorKindHuman {
		return true
	}
	return a.Kind == requestActorKindBroker && strings.TrimSpace(actor) == ""
}

func writeAppError(w http.ResponseWriter, err error) {
	switch {
	case isCustomAppCallerError(err):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.Is(err, os.ErrNotExist):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}

// Delete removes an app directory. Returns a caller error for an unknown id so
// the handler maps it to 404 rather than 500.
func (s *customAppStore) Delete(id string) error {
	if err := validateCustomAppID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := s.appDir(id)
	if _, err := os.Stat(filepath.Join(dir, customAppManifestFile)); err != nil {
		if os.IsNotExist(err) {
			return newCustomAppCallerError("app: %s not found", id)
		}
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	// Evict the per-app publish mutex so the publishMu map doesn't grow without
	// bound across create/delete cycles. Safe: the dir is gone, so no publish for
	// this id can be in flight or start; a future same-id app lazily re-creates it.
	s.publishMu.Delete(id)
	return nil
}
