package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/cloud/internal/domain"
	"github.com/aoagents/agent-orchestrator/cloud/internal/linear"
	"github.com/aoagents/agent-orchestrator/cloud/internal/postgres"
)

type linearHTTPStore struct {
	Store
	state linear.State
}

func (s *linearHTTPStore) PrincipalFromLocalToken(context.Context, []byte) (domain.Principal, error) {
	return domain.Principal{UserID: "owner"}, nil
}
func (s *linearHTTPStore) LinearUpdate(_ context.Context, p *domain.Principal, org string, fn func(*linear.State) error) error {
	if org != "org" || (p != nil && p.UserID != "owner") {
		return postgres.ErrForbidden
	}
	s.state.Init()
	return fn(&s.state)
}
func (s *linearHTTPStore) LinearRoute(_ context.Context, kind, key string) (string, error) {
	if kind == "worker" && key == linear.Hash(strings.Repeat("x", 32)) {
		return "org", nil
	}
	return "", postgres.ErrNotFound
}
func (s *linearHTTPStore) LinearOrganizations(context.Context) ([]string, error) {
	return []string{"org"}, nil
}
func TestLinearRoutesAuthenticationAndSecretRedaction(t *testing.T) {
	store := &linearHTTPStore{}
	store.state.Init()
	store.state.Connections["c"] = &linear.Connection{ID: "c", Encrypted: []byte("provider-secret"), Nonce: []byte("nonce")}
	store.state.Profiles["p"] = &linear.Profile{ID: "p", ConnectionID: "c", Owner: "owner", Challenge: linear.Hash(strings.Repeat("x", 32))}
	server := New(Options{Store: store, LocalAuthEnabled: true, Linear: &linear.Service{Store: store}})
	request := func(path, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, r)
		return w
	}
	if w := request("/api/cloud/v1/orgs/org/linear", ""); w.Code != 401 {
		t.Fatal("unauthenticated settings", w.Code, w.Body.String())
	}
	if w := request("/api/cloud/v1/orgs/other/linear", "ao_local_test"); w.Code != 403 {
		t.Fatal("cross-org settings", w.Code, w.Body.String())
	}
	w := request("/api/cloud/v1/orgs/org/linear", "ao_local_test")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "encrypted") || strings.Contains(w.Body.String(), "nonce") || strings.Contains(w.Body.String(), store.state.Profiles["p"].Challenge) {
		t.Fatal("secret material in settings", w.Body.String())
	}
	if w = request("/api/cloud/v1/linear/worker/status", "wrong"); w.Code != 401 {
		t.Fatal("unauthorized worker", w.Code)
	}
	w = request("/api/cloud/v1/linear/worker/status", strings.Repeat("x", 32))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var p linear.Profile
	json.Unmarshal(w.Body.Bytes(), &p)
	if p.ID != "p" || p.Challenge != "" {
		t.Fatal("bad public worker metadata")
	}
}
func TestLinearWorkerCannotAckAnotherProfilesCommand(t *testing.T) {
	store := &linearHTTPStore{}
	store.state.Init()
	store.state.Profiles["p"] = &linear.Profile{ID: "p", Challenge: linear.Hash(strings.Repeat("x", 32))}
	store.state.Tasks["task"] = &linear.Task{ProfileID: "other", Commands: []*linear.Command{{ID: "cmd", Task: "task", Lease: "lease"}}}
	server := New(Options{Store: store, Linear: &linear.Service{Store: store}})
	r := httptest.NewRequest("POST", "/api/cloud/v1/linear/worker/ack", strings.NewReader(`{"id":"cmd","task":"task","lease":"lease"}`))
	r.Header.Set("Authorization", "Bearer "+strings.Repeat("x", 32))
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, r)
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	if store.state.Tasks["task"].Commands[0].Done {
		t.Fatal("cross-runner ack accepted")
	}
}
