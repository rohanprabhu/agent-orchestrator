package controllers

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	linear "github.com/aoagents/agent-orchestrator/backend/internal/service/linearintegration"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
)

// LinearController exposes loopback-only local runner management.
type LinearController struct {
	Manager  *linear.Manager
	Projects projectsvc.Manager
}

// Register mounts the daemon-owned Linear runner routes.
func (c *LinearController) Register(r chi.Router) {
	r.Get("/settings/integrations/linear/runners", c.list)
	r.Post("/settings/integrations/linear/runners", c.prepare)
	r.Post("/settings/integrations/linear/runners/{runnerId}/activate", c.activate)
	r.Delete("/settings/integrations/linear/runners/{runnerId}", c.disconnect)
}
func (c *LinearController) ready(w http.ResponseWriter, r *http.Request) bool {
	if c.Manager == nil {
		apispec.NotImplemented(w, r, r.Method, r.URL.Path)
		return false
	}
	return true
}
func linearRunner(r linear.Runner) LinearRunnerResponse {
	return LinearRunnerResponse{ID: r.ID, ProfileID: r.ProfileID, ProjectID: r.ProjectID, Challenge: r.Challenge, Active: r.Active, LastContact: r.LastContact, Error: r.Error}
}
func linearError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, linear.ErrInvalid) {
		envelope.WriteAPIError(w, r, 400, "validation", "LINEAR_RUNNER_INVALID", err.Error(), nil)
	} else {
		envelope.WriteError(w, r, err)
	}
}
func (c *LinearController) list(w http.ResponseWriter, r *http.Request) {
	if !c.ready(w, r) {
		return
	}
	out := LinearRunnersResponse{Runners: []LinearRunnerResponse{}}
	for _, runner := range c.Manager.List() {
		out.Runners = append(out.Runners, linearRunner(runner))
	}
	envelope.WriteJSON(w, 200, out)
}
func (c *LinearController) prepare(w http.ResponseWriter, r *http.Request) {
	if !c.ready(w, r) {
		return
	}
	var body PrepareLinearRunnerRequest
	if !decodeConversationBody(w, r, &body) {
		return
	}
	if c.Projects == nil {
		envelope.WriteAPIError(w, r, 503, "unavailable", "PROJECTS_UNAVAILABLE", "Project service unavailable", nil)
		return
	}
	if _, err := c.Projects.Get(r.Context(), domain.ProjectID(body.ProjectID)); err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	runner, err := c.Manager.Prepare(body.ProjectID)
	if err != nil {
		linearError(w, r, err)
		return
	}
	envelope.WriteJSON(w, 201, linearRunner(runner))
}
func (c *LinearController) activate(w http.ResponseWriter, r *http.Request) {
	if !c.ready(w, r) {
		return
	}
	var body ActivateLinearRunnerRequest
	if !decodeConversationBody(w, r, &body) {
		return
	}
	runner, err := c.Manager.Activate(r.Context(), chi.URLParam(r, "runnerId"), body.ProfileID)
	if err != nil {
		linearError(w, r, err)
		return
	}
	envelope.WriteJSON(w, 200, linearRunner(runner))
}
func (c *LinearController) disconnect(w http.ResponseWriter, r *http.Request) {
	if !c.ready(w, r) {
		return
	}
	if err := c.Manager.Disconnect(chi.URLParam(r, "runnerId")); err != nil {
		linearError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
