package linearbridge

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Server is the standalone single-workspace pilot coordinator.
type Server struct {
	Store                                              *Store
	WebhookSecret, WorkerToken, OrganizationID, TeamID string
	Linear                                             *Linear
}

type webhook struct {
	Type           string `json:"type"`
	Action         string `json:"action"`
	OrganizationID string `json:"organizationId"`
	Timestamp      int64  `json:"webhookTimestamp"`
	PromptContext  string `json:"promptContext"`
	Session        struct {
		ID    string `json:"id"`
		Issue *struct {
			ID         string `json:"id"`
			Identifier string `json:"identifier"`
			Team       struct {
				ID string `json:"id"`
			} `json:"team"`
		} `json:"issue"`
	} `json:"agentSession"`
	Activity struct {
		ID      string `json:"id"`
		Body    string `json:"body"`
		Signal  string `json:"signal"`
		Content struct {
			Type string `json:"type"`
			Body string `json:"body"`
		} `json:"content"`
	} `json:"agentActivity"`
}

// Handler validates configuration and builds the webhook/worker routes.
func (s *Server) Handler() (http.Handler, error) {
	if s.Store == nil || len(s.WorkerToken) < 32 || s.WebhookSecret == "" || s.OrganizationID == "" || s.TeamID == "" {
		return nil, errors.New("store, webhook secret, 32-character worker token, organization and team are required")
	}
	if err := s.Store.pinScope(context.Background(), "coordinator:"+s.OrganizationID+":"+s.TeamID); err != nil {
		return nil, err
	}
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	m.HandleFunc("POST /linear/webhook", s.webhook)
	m.HandleFunc("GET /worker/commands", s.auth(func(w http.ResponseWriter, r *http.Request) {
		c, err := s.Store.claim(r.Context())
		if err != nil {
			http.Error(w, "queue unavailable", http.StatusServiceUnavailable)
			return
		}
		if c == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, c)
	}))
	m.HandleFunc("POST /worker/check", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var c Command
		if !decode(w, r, &c) {
			return
		}
		var count int
		err := s.Store.db.QueryRowContext(r.Context(), `SELECT count(*) FROM commands WHERE id=? AND lease=? AND done=0 AND until_ms>?`, c.ID, c.Lease, time.Now().UnixMilli()).Scan(&count)
		if err != nil {
			http.Error(w, "queue unavailable", http.StatusServiceUnavailable)
			return
		}
		if count != 1 {
			http.Error(w, "command expired or superseded", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	m.HandleFunc("POST /worker/ack", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var c Command
		if !decode(w, r, &c) {
			return
		}
		if err := s.Store.ack(r.Context(), c); err != nil {
			http.Error(w, "command expired or superseded", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	m.HandleFunc("POST /worker/reports", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var report Report
		if !decode(w, r, &report) {
			return
		}
		if report.ID == "" || report.Task == "" || strings.TrimSpace(report.Body) == "" || len(report.Body) > 24000 || !activityKind(report.Kind) {
			http.Error(w, "invalid report", http.StatusBadRequest)
			return
		}
		if err := s.Store.report(r.Context(), report); err != nil {
			http.Error(w, "queue unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	return m, nil
}
func activityKind(s string) bool {
	return s == "thought" || s == "response" || s == "error" || s == "elicitation"
}
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(header, "Bearer ")
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.WorkerToken)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	if err := d.Decode(v); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return false
	}
	return true
}
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	signature, err := hex.DecodeString(r.Header.Get("Linear-Signature"))
	mac := hmac.New(sha256.New, []byte(s.WebhookSecret))
	_, _ = mac.Write(raw)
	if err != nil || !hmac.Equal(signature, mac.Sum(nil)) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	var event webhook
	if json.Unmarshal(raw, &event) != nil {
		http.Error(w, "invalid event", http.StatusBadRequest)
		return
	}
	age := time.Since(time.UnixMilli(event.Timestamp))
	if age > time.Minute || age < -time.Minute {
		http.Error(w, "expired event", http.StatusUnauthorized)
		return
	}
	if event.OrganizationID != s.OrganizationID {
		http.Error(w, "workspace not configured", http.StatusForbidden)
		return
	}
	if event.Type != "AgentSessionEvent" {
		w.WriteHeader(http.StatusOK)
		return
	}
	c := Command{Task: event.Session.ID}
	if c.Task == "" {
		http.Error(w, "missing session", http.StatusBadRequest)
		return
	}
	switch event.Action {
	case "created":
		if event.Session.Issue == nil || event.Session.Issue.Team.ID != s.TeamID {
			http.Error(w, "issue team not configured", http.StatusForbidden)
			return
		}
		if strings.TrimSpace(event.PromptContext) == "" {
			http.Error(w, "missing prompt context", http.StatusBadRequest)
			return
		}
		c.ID = c.Task + ":created"
		c.Kind = "create"
		c.Body = event.PromptContext
	case "prompted":
		if event.Activity.ID == "" {
			http.Error(w, "missing activity", http.StatusBadRequest)
			return
		}
		c.ID = c.Task + ":" + event.Activity.ID
		c.Kind = "message"
		c.Body = event.Activity.Content.Body
		if c.Body == "" {
			c.Body = event.Activity.Body
		}
		if event.Activity.Signal == "stop" {
			c.Kind = "stop"
		} else if strings.TrimSpace(c.Body) == "" {
			http.Error(w, "empty prompt", http.StatusBadRequest)
			return
		}
	default:
		w.WriteHeader(http.StatusOK)
		return
	}
	if err := s.Store.enqueue(r.Context(), c); err != nil {
		http.Error(w, "queue unavailable or session not received yet", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// RunOutbox is independent of webhook requests, so Linear latency cannot delay
// webhook acknowledgement. Run exactly one publisher per coordinator database.
func (s *Server) RunOutbox(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := s.publish(ctx); err != nil {
				slog.Warn("Linear outbox delivery failed; will retry")
			}
		}
	}
}
func (s *Server) publish(ctx context.Context) error {
	var r Report
	err := s.Store.db.QueryRowContext(ctx, `SELECT id,task,kind,body FROM outbox WHERE done=0 ORDER BY seq LIMIT 1`).Scan(&r.ID, &r.Task, &r.Kind, &r.Body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.Linear.Publish(ctx, r); err != nil {
		return err
	}
	_, err = s.Store.db.ExecContext(ctx, `UPDATE outbox SET done=1 WHERE id=?`, r.ID)
	return err
}
