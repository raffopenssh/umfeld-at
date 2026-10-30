// api_feedback.go — LLM feedback endpoint for bug reports and feature requests.
//
// Agents can POST structured feedback and GET existing entries.
// Stored in a local SQLite database (data/feedback.db).
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// ═══════════════════════════════════════════════════════════════════════════════
// Feedback store
// ═══════════════════════════════════════════════════════════════════════════════

const feedbackDBPath = "data/feedback.db"

var (
	feedbackDB   *sql.DB
	feedbackOnce sync.Once
)

func initFeedbackDB() error {
	var initErr error
	feedbackOnce.Do(func() {
		db, err := sql.Open("sqlite3", feedbackDBPath+"?_journal_mode=WAL")
		if err != nil {
			initErr = err
			return
		}
		_, err = db.Exec(`
			CREATE TABLE IF NOT EXISTS feedback (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
				agent       TEXT NOT NULL DEFAULT '',
				category    TEXT NOT NULL CHECK(category IN ('bug', 'feature', 'improvement', 'question', 'other')),
				severity    TEXT NOT NULL DEFAULT 'normal' CHECK(severity IN ('critical', 'high', 'normal', 'low')),
				status      TEXT NOT NULL DEFAULT 'open' CHECK(status IN ('open', 'acknowledged', 'in_progress', 'resolved', 'wontfix')),
				title       TEXT NOT NULL,
				description TEXT NOT NULL DEFAULT '',
				endpoint    TEXT NOT NULL DEFAULT '',
				context     TEXT NOT NULL DEFAULT '{}',
				resolution  TEXT NOT NULL DEFAULT ''
			);
			CREATE INDEX IF NOT EXISTS idx_feedback_status ON feedback(status);
			CREATE INDEX IF NOT EXISTS idx_feedback_category ON feedback(category);
		`)
		if err != nil {
			initErr = err
			return
		}
		feedbackDB = db
		log.Println("📋 Feedback database initialized")
	})
	return initErr
}

// ═══════════════════════════════════════════════════════════════════════════════
// HTTP handler
// ═══════════════════════════════════════════════════════════════════════════════

func init() {
	handleV1Feedback = feedbackHandler
}

func feedbackHandler(w http.ResponseWriter, r *http.Request) {
	if err := initFeedbackDB(); err != nil {
		apiError(w, "feedback database unavailable: "+err.Error(), 503)
		return
	}

	// Route by method, with optional /id suffix for PATCH
	path := r.URL.Path
	switch r.Method {
	case http.MethodPost:
		feedbackCreate(w, r)
	case http.MethodGet:
		feedbackList(w, r)
	case http.MethodPatch:
		// PATCH /api/v1/feedback/{id}
		idStr := r.PathValue("id")
		if idStr == "" {
			// Fallback: extract from path
			parts := strings.Split(strings.TrimSuffix(path, "/"), "/")
			if len(parts) > 0 {
				idStr = parts[len(parts)-1]
			}
		}
		if idStr == "" || idStr == "feedback" {
			apiError(w, "PATCH requires an ID: /api/v1/feedback/{id}", 400)
			return
		}
		feedbackUpdate(w, r, idStr)
	default:
		apiError(w, "method not allowed, use GET, POST or PATCH", 405)
	}
}

// ─── POST /api/v1/feedback ───────────────────────────────────────────────────

type feedbackRequest struct {
	Agent       string                 `json:"agent"`
	Category    string                 `json:"category"`
	Severity    string                 `json:"severity"`
	Title       string                 `json:"title"`
	Description string                 `json:"description"`
	Endpoint    string                 `json:"endpoint"`
	Context     map[string]interface{} `json:"context"`
}

func feedbackCreate(w http.ResponseWriter, r *http.Request) {
	var req feedbackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, "invalid JSON body: "+err.Error(), 400)
		return
	}

	// Validate required fields
	if req.Title == "" {
		apiError(w, "title is required", 400)
		return
	}

	if req.Category == "" {
		req.Category = "other"
	}
	validCategories := map[string]bool{"bug": true, "feature": true, "improvement": true, "question": true, "other": true}
	if !validCategories[req.Category] {
		apiError(w, fmt.Sprintf("category must be one of: bug, feature, improvement, question, other (got '%s')", req.Category), 400)
		return
	}
	if req.Severity == "" {
		req.Severity = "normal"
	}
	validSeverities := map[string]bool{"critical": true, "high": true, "normal": true, "low": true}
	if !validSeverities[req.Severity] {
		apiError(w, fmt.Sprintf("severity must be one of: critical, high, normal, low (got '%s')", req.Severity), 400)
		return
	}

	contextJSON, _ := json.Marshal(req.Context)
	if req.Context == nil {
		contextJSON = []byte("{}")
	}

	result, err := feedbackDB.Exec(`
		INSERT INTO feedback (agent, category, severity, title, description, endpoint, context)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		req.Agent, req.Category, req.Severity, req.Title, req.Description, req.Endpoint, string(contextJSON))
	if err != nil {
		apiError(w, "failed to save feedback: "+err.Error(), 500)
		return
	}

	id, _ := result.LastInsertId()
	log.Printf("📋 New feedback #%d from '%s': [%s/%s] %s", id, req.Agent, req.Category, req.Severity, req.Title)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "created",
		"id":      id,
		"message": fmt.Sprintf("Feedback #%d recorded. Thank you!", id),
	})
}

// ─── PATCH /api/v1/feedback/{id} ───────────────────────────────────────────────

type feedbackUpdateRequest struct {
	Status     string `json:"status"`
	Resolution string `json:"resolution"`
}

func feedbackUpdate(w http.ResponseWriter, r *http.Request, idStr string) {
	var id int64
	fmt.Sscanf(idStr, "%d", &id)
	if id == 0 {
		apiError(w, "invalid feedback ID", 400)
		return
	}

	var req feedbackUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, "invalid JSON body: "+err.Error(), 400)
		return
	}

	validStatuses := map[string]bool{"open": true, "acknowledged": true, "in_progress": true, "resolved": true, "wontfix": true}
	if req.Status != "" && !validStatuses[req.Status] {
		apiError(w, "status must be one of: open, acknowledged, in_progress, resolved, wontfix", 400)
		return
	}

	var sets []string
	var args []interface{}
	if req.Status != "" {
		sets = append(sets, "status = ?")
		args = append(args, req.Status)
	}
	if req.Resolution != "" {
		sets = append(sets, "resolution = ?")
		args = append(args, req.Resolution)
	}
	if len(sets) == 0 {
		apiError(w, "nothing to update — provide status and/or resolution", 400)
		return
	}
	args = append(args, id)

	res, err := feedbackDB.Exec("UPDATE feedback SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	if err != nil {
		apiError(w, "update failed: "+err.Error(), 500)
		return
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		apiError(w, fmt.Sprintf("feedback #%d not found", id), 404)
		return
	}

	log.Printf("📋 Feedback #%d updated: status=%s", id, req.Status)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "updated",
		"id":     id,
	})
}

// ─── GET /api/v1/feedback ────────────────────────────────────────────────────

func feedbackList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// Build query with optional filters
	var wheres []string
	var args []interface{}

	if status := q.Get("status"); status != "" {
		wheres = append(wheres, "status = ?")
		args = append(args, status)
	}
	if category := q.Get("category"); category != "" {
		wheres = append(wheres, "category = ?")
		args = append(args, category)
	}
	if severity := q.Get("severity"); severity != "" {
		wheres = append(wheres, "severity = ?")
		args = append(args, severity)
	}
	if agent := q.Get("agent"); agent != "" {
		wheres = append(wheres, "agent = ?")
		args = append(args, agent)
	}

	query := "SELECT id, created_at, agent, category, severity, status, title, description, endpoint, context, resolution FROM feedback"
	if len(wheres) > 0 {
		query += " WHERE " + strings.Join(wheres, " AND ")
	}
	query += " ORDER BY id DESC"

	limit := 100
	if l := q.Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
		if limit > 1000 {
			limit = 1000
		}
	}
	query += fmt.Sprintf(" LIMIT %d", limit)

	rows, err := feedbackDB.Query(query, args...)
	if err != nil {
		apiError(w, "query failed: "+err.Error(), 500)
		return
	}
	defer rows.Close()

	var items []map[string]interface{}
	for rows.Next() {
		var id int64
		var createdAt, agent, category, severity, status, title, desc, endpoint, contextStr, resolution string
		if err := rows.Scan(&id, &createdAt, &agent, &category, &severity, &status, &title, &desc, &endpoint, &contextStr, &resolution); err != nil {
			continue
		}
		var ctx interface{}
		json.Unmarshal([]byte(contextStr), &ctx)

		item := map[string]interface{}{
			"id":          id,
			"created_at":  createdAt,
			"agent":       agent,
			"category":    category,
			"severity":    severity,
			"status":      status,
			"title":       title,
			"description": desc,
			"endpoint":    endpoint,
			"context":     ctx,
		}
		if resolution != "" {
			item["resolution"] = resolution
		}
		items = append(items, item)
	}
	if items == nil {
		items = []map[string]interface{}{}
	}

	// Summary counts
	var openCount, totalCount int
	feedbackDB.QueryRow("SELECT COUNT(*) FROM feedback").Scan(&totalCount)
	feedbackDB.QueryRow("SELECT COUNT(*) FROM feedback WHERE status = 'open'").Scan(&openCount)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"feedback": items,
		"meta": map[string]interface{}{
			"total":     totalCount,
			"open":      openCount,
			"returned":  len(items),
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		},
	})
}
