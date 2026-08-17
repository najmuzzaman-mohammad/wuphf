package team

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// handleVisualArtifacts owns the collection route:
//
//	GET  /visual-artifacts?slug=&source_path=
//	POST /visual-artifacts
func (b *Broker) handleVisualArtifacts(w http.ResponseWriter, r *http.Request) {
	worker := b.requireWikiWorker(w, "visual artifact")
	if worker == nil {
		return
	}
	switch r.Method {
	case http.MethodGet:
		// owner_slug is the frontend-facing query name; slug is the legacy
		// agent-facing name. Accept both so MCP + UI callers can share the
		// same endpoint without translation.
		createdBy := strings.TrimSpace(r.URL.Query().Get("slug"))
		if createdBy == "" {
			createdBy = strings.TrimSpace(r.URL.Query().Get("owner_slug"))
		}
		// source_path is the FE-facing query name; source_markdown_path is
		// the JSON-payload-shaped alias spec'd for the listing contract.
		// Accept both so MCP + UI callers (and any future SDK) can use the
		// shape they already speak.
		sourcePath := strings.TrimSpace(r.URL.Query().Get("source_path"))
		if sourcePath == "" {
			sourcePath = strings.TrimSpace(r.URL.Query().Get("source_markdown_path"))
		}
		filter := RichArtifactFilter{
			CreatedBy:          createdBy,
			SourceMarkdownPath: sourcePath,
		}
		if filter.CreatedBy != "" {
			if err := validateNotebookSlug(filter.CreatedBy); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
		}
		if filter.SourceMarkdownPath != "" {
			if err := validateNotebookPath(filter.SourceMarkdownPath); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
		}
		artifacts, err := worker.ListRichArtifacts(filter)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"artifacts": artifacts})
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 512*1024)
		var body RichArtifactCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		slug, status, err := richArtifactAuthenticatedSlug(r, body.Slug, "slug")
		if err != nil {
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		body.Slug = slug
		artifact, html, err := newRichArtifact(body, time.Now())
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		stored, sha, n, err := worker.CreateRichArtifact(r.Context(), artifact, html, body.CommitMessage)
		if err != nil {
			writeRichArtifactError(w, err)
			return
		}
		// stored carries the canonical notebook-home attachment chosen by the
		// worker. Always pass it through DerivePromotion so the response shape
		// matches the list/get endpoints (non-nil promotion field).
		writeJSON(w, http.StatusOK, map[string]any{
			"artifact":      stored.WithDerivedPromotion(),
			"commit_sha":    sha,
			"bytes_written": n,
		})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleVisualArtifactSubpath owns:
//
//	GET  /visual-artifacts/{id}
//	POST /visual-artifacts/{id}/promote
func (b *Broker) handleVisualArtifactSubpath(w http.ResponseWriter, r *http.Request) {
	worker := b.requireWikiWorker(w, "visual artifact")
	if worker == nil {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/visual-artifacts/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "visual artifact not found"})
		return
	}
	id := parts[0]
	if err := validateRichArtifactID(id); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		artifact, html, err := worker.RichArtifact(id)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"artifact": artifact, "html": html})
		return
	}
	if len(parts) == 2 && parts[1] == "promote" {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 512*1024)
		var body RichArtifactPromoteRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		actorSlug, status, err := richArtifactAuthenticatedSlug(r, body.ActorSlug, "actor_slug")
		if err != nil {
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		if actorSlug == "" {
			actorSlug = "human"
		}
		artifact, sha, n, err := worker.PromoteRichArtifact(
			r.Context(),
			actorSlug,
			id,
			strings.TrimSpace(body.TargetWikiPath),
			body.MarkdownSummary,
			strings.TrimSpace(body.Mode),
			body.CommitMessage,
		)
		if err != nil {
			writeRichArtifactError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"artifact":      artifact,
			"commit_sha":    sha,
			"bytes_written": n,
		})
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "visual artifact not found"})
}

func richArtifactAuthenticatedSlug(r *http.Request, bodySlug, fieldName string) (string, int, error) {
	bodySlug = strings.TrimSpace(bodySlug)
	agentSlug := strings.TrimSpace(r.Header.Get(agentRateLimitHeader))
	if agentSlug != "" {
		if err := validateNotebookSlug(agentSlug); err != nil {
			return "", http.StatusBadRequest, err
		}
		if bodySlug != "" && bodySlug != agentSlug {
			return "", http.StatusForbidden, errors.New(fieldName + " does not match authenticated agent")
		}
		if actor, ok := requestActorFromContext(r.Context()); ok && actor.Kind == requestActorKindHuman {
			humanSlug := humanIdentityFromActor(actor).Slug
			if humanSlug != "" && humanSlug != agentSlug {
				return "", http.StatusForbidden, errors.New("session identity does not match authenticated agent")
			}
		}
		return agentSlug, 0, nil
	}
	if actor, ok := requestActorFromContext(r.Context()); ok && actor.Kind == requestActorKindHuman {
		return humanIdentityFromActor(actor).Slug, 0, nil
	}
	return bodySlug, 0, nil
}

// handleWikiVisualArtifact returns the promoted visual artifact associated with
// a wiki article, when one exists.
//
//	GET /wiki/visual?path=team/decisions/foo.md
func (b *Broker) handleWikiVisualArtifact(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	worker := b.requireWikiWorker(w, "visual artifact")
	if worker == nil {
		return
	}
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if err := validateArticlePath(path); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	artifacts, err := worker.ListRichArtifacts(RichArtifactFilter{PromotedWikiPath: path})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if len(artifacts) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "visual artifact not found"})
		return
	}
	artifact, html, err := worker.RichArtifact(artifacts[0].ID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifact": artifact, "html": html})
}

func writeRichArtifactError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrQueueSaturated):
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrWorkerStopped):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
	case isRichArtifactCallerError(err):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}

func isRichArtifactCallerError(err error) bool {
	return errors.Is(err, errRichArtifactCaller)
}
