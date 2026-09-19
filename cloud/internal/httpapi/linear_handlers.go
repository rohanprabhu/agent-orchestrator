package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/cloud/internal/linear"
	"github.com/go-chi/chi/v5"
)

func (s *Server) linearError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, linear.ErrConflict):
		http.Error(w, err.Error()+". Choose another name or scope.", 409)
	case errors.Is(err, linear.ErrInvalid), errors.Is(err, linear.ErrExpired):
		http.Error(w, err.Error(), 400)
	default:
		s.writeStoreError(w, r, err)
	}
}
func linearDecode(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		http.Error(w, "invalid JSON", 400)
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		http.Error(w, "invalid JSON", 400)
		return false
	}
	return true
}
func (s *Server) linearSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.linear == nil {
		writeJSON(w, 200, map[string]any{"available": false, "connections": []any{}, "profiles": []any{}})
		return
	}
	p := principalFrom(r)
	connections := []any{}
	profiles := []linear.Profile{}
	err := s.linear.Store.LinearUpdate(r.Context(), &p, chi.URLParam(r, "orgId"), func(st *linear.State) error {
		for _, c := range st.Connections {
			connections = append(connections, map[string]any{"id": c.ID, "workspace": c.Workspace, "agent": c.Agent, "teams": c.Teams, "error": c.Error})
		}
		for _, v := range st.Profiles {
			if v.Disconnected {
				continue
			}
			copy := *v
			copy.Challenge = ""
			profiles = append(profiles, copy)
		}
		return nil
	})
	if err != nil {
		s.linearError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"available": true, "connections": connections, "profiles": profiles})
}
func (s *Server) linearConnect(w http.ResponseWriter, r *http.Request) {
	if s.linear == nil {
		http.Error(w, "Linear is not configured on this service", 503)
		return
	}
	link, err := s.linear.Connect(r.Context(), principalFrom(r), chi.URLParam(r, "orgId"))
	if err != nil {
		s.linearError(w, r, err)
		return
	}
	writeJSON(w, 201, map[string]string{"authorizationUrl": link})
}
func (s *Server) linearCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if err := s.linear.Callback(r.Context(), r.URL.Query().Get("state"), r.URL.Query().Get("code")); err != nil {
		http.Error(w, "Linear connection did not complete. Return to AO Settings and try again.", 400)
		return
	}
	_, _ = io.WriteString(w, "Linear is connected. Return to AO Settings → Integrations to choose a team and enable your local runner.")
}
func (s *Server) linearProjects(w http.ResponseWriter, r *http.Request) {
	if s.linear == nil {
		http.Error(w, "Linear unavailable", 503)
		return
	}
	projects, err := s.linear.Projects(r.Context(), principalFrom(r), chi.URLParam(r, "orgId"), r.URL.Query().Get("connectionId"), r.URL.Query().Get("teamId"))
	if err != nil {
		s.linearError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"projects": projects})
}
func (s *Server) linearProfile(w http.ResponseWriter, r *http.Request) {
	if s.linear == nil {
		http.Error(w, "Linear unavailable", 503)
		return
	}
	var profile linear.Profile
	if !linearDecode(w, r, &profile) {
		return
	}
	saved, err := s.linear.SaveProfile(r.Context(), principalFrom(r), chi.URLParam(r, "orgId"), profile)
	if err != nil {
		s.linearError(w, r, err)
		return
	}
	copy := *saved
	copy.Challenge = ""
	writeJSON(w, 200, copy)
}
func (s *Server) linearDisconnect(w http.ResponseWriter, r *http.Request) {
	if s.linear == nil {
		http.Error(w, "Linear unavailable", 503)
		return
	}
	p := principalFrom(r)
	err := s.linear.Store.LinearUpdate(r.Context(), &p, chi.URLParam(r, "orgId"), func(st *linear.State) error {
		profile := st.Profiles[chi.URLParam(r, "profileId")]
		if profile == nil {
			return linear.ErrInvalid
		}
		profile.Paused = true
		profile.Disconnected = true
		return nil
	})
	if err != nil {
		s.linearError(w, r, err)
		return
	}
	w.WriteHeader(204)
}
func (s *Server) linearWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "invalid body", 400)
		return
	}
	if err = s.linear.Webhook(r.Context(), body, r.Header.Get("Linear-Signature")); err != nil {
		http.Error(w, "webhook rejected", 400)
		return
	}
	w.WriteHeader(200)
}
func (s *Server) linearWorker(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") || len(header) < 39 {
		http.Error(w, "unauthorized", 401)
		return
	}
	hash := linear.Hash(strings.TrimPrefix(header, "Bearer "))
	org, err := s.linear.Store.LinearRoute(r.Context(), "worker", hash)
	if err != nil {
		http.Error(w, "unauthorized", 401)
		return
	}
	var result any
	status := 204
	err = s.linear.Store.LinearUpdate(r.Context(), nil, org, func(st *linear.State) error {
		var profile *linear.Profile
		for _, v := range st.Profiles {
			if !v.Disconnected && v.Challenge == hash {
				profile = v
			}
		}
		if profile == nil {
			return linear.ErrInvalid
		}
		profile.LastSeen = time.Now()
		switch chi.URLParam(r, "operation") {
		case "status":
			if r.Method != "GET" {
				status = 405
				return nil
			}
			copy := *profile
			copy.Challenge = ""
			result = copy
			status = 200
		case "commands":
			if r.Method != "GET" {
				status = 405
				return nil
			}
			c := st.Claim(profile.ID, time.Now())
			if c != nil {
				result = c
				status = 200
			}
		case "check", "ack":
			if r.Method != "POST" {
				status = 405
				return nil
			}
			var c linear.Command
			if !linearDecode(w, r, &c) {
				status = 0
				return nil
			}
			if !st.Check(profile.ID, c, chi.URLParam(r, "operation") == "ack", time.Now()) {
				status = 409
			}
		case "reports":
			if r.Method != "POST" {
				status = 405
				return nil
			}
			var report linear.Report
			if !linearDecode(w, r, &report) {
				status = 0
				return nil
			}
			t := st.Tasks[report.Task]
			if t == nil || t.ProfileID != profile.ID || report.ID == "" || len(report.ID) > 512 || len(report.Body) > 24000 || strings.TrimSpace(report.Body) == "" || (report.Kind != "thought" && report.Kind != "response" && report.Kind != "error" && report.Kind != "elicitation") {
				return linear.ErrInvalid
			}
			// Scope report IDs to this profile so another runner cannot suppress delivery.
			report.ID = profile.ID + ":" + report.ID
			st.Report(report)
		default:
			status = 404
		}
		return nil
	})
	if err != nil {
		s.linearError(w, r, err)
		return
	}
	if status == 0 {
		return
	}
	if result != nil {
		writeJSON(w, status, result)
	} else {
		w.WriteHeader(status)
	}
}
