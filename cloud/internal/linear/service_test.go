package linear

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/cloud/internal/domain"
	"github.com/aoagents/agent-orchestrator/cloud/internal/secrets"
)

type memoryStore struct {
	mu     sync.Mutex
	states map[string]*State
	deny   bool
}

func (m *memoryStore) LinearUpdate(_ context.Context, p *domain.Principal, org string, fn func(*State) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p != nil && m.deny {
		return errors.New("forbidden")
	}
	raw, _ := json.Marshal(m.states[org])
	st := &State{}
	json.Unmarshal(raw, &st)
	if st == nil {
		st = &State{}
	}
	st.Init()
	if err := fn(st); err != nil {
		return err
	}
	m.states[org] = st
	return nil
}
func (m *memoryStore) LinearRoute(_ context.Context, kind, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for org, s := range m.states {
		if kind == "oauth" && s.Attempts[key] != nil {
			return org, nil
		}
		for _, c := range s.Connections {
			if kind == "workspace" && c.Workspace.ID == key {
				return org, nil
			}
		}
	}
	return "", ErrInvalid
}
func (m *memoryStore) LinearOrganizations(context.Context) ([]string, error) {
	return []string{"org"}, nil
}
func testService(t *testing.T) (*Service, *memoryStore, *httptest.Server) {
	t.Helper()
	cipher, _ := secrets.New(make([]byte, 32))
	m := &memoryStore{states: map[string]*State{}}
	s, err := New(m, cipher, Config{ClientID: "client", ClientSecret: "secret", RedirectURL: "https://ao.test/callback", WebhookSecret: "signing"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			r.ParseForm()
			if r.Form.Get("grant_type") == "authorization_code" && r.Form.Get("code_verifier") == "" {
				t.Error("missing PKCE")
			}
			w.Write([]byte(`{"access_token":"access","refresh_token":"refresh","expires_in":86400}`))
			return
		}
		var req struct{ Query string }
		json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "organization"):
			w.Write([]byte(`{"data":{"organization":{"id":"workspace","name":"Acme"},"viewer":{"id":"agent","name":"AO","displayName":"ao2"}}}`))
		case strings.Contains(req.Query, "teams("):
			w.Write([]byte(`{"data":{"teams":{"nodes":[{"id":"team","name":"Engineering"}],"pageInfo":{"hasNextPage":false}}}}`))
		default:
			w.Write([]byte(`{"errors":[{"message":"forbidden"}]}`))
		}
	}))
	t.Cleanup(server.Close)
	s.TokenURL = server.URL + "/token"
	s.GraphQLURL = server.URL + "/graphql"
	return s, m, server
}
func TestOAuthSingleUseEncryptedAndMembershipRechecked(t *testing.T) {
	s, m, _ := testService(t)
	ctx := context.Background()
	link, err := s.Connect(ctx, domain.Principal{UserID: "u"}, "org")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(link)
	state := u.Query().Get("state")
	if u.Query().Get("actor") != "app" || u.Query().Get("code_challenge") == "" {
		t.Fatal("agent OAuth configuration missing")
	}
	if err = s.Callback(ctx, state, "code"); err != nil {
		t.Fatal(err)
	}
	if err = s.Callback(ctx, state, "code"); !errors.Is(err, ErrExpired) {
		t.Fatal("OAuth replay accepted")
	}
	raw, _ := json.Marshal(m.states["org"])
	if strings.Contains(string(raw), `"access"`) || strings.Contains(string(raw), `"refresh"`) {
		t.Fatal("plaintext provider credential persisted")
	}
	for _, c := range m.states["org"].Connections {
		if c.Agent.DisplayName != "ao2" {
			t.Fatal("actual provider handle lost")
		}
	}
	link, _ = s.Connect(ctx, domain.Principal{UserID: "u"}, "org")
	u, _ = url.Parse(link)
	m.deny = true
	if s.Callback(ctx, u.Query().Get("state"), "code") == nil {
		t.Fatal("revoked membership accepted")
	}
}
func TestOAuthCancelAndExpiry(t *testing.T) {
	s, m, _ := testService(t)
	ctx := context.Background()
	link, _ := s.Connect(ctx, domain.Principal{UserID: "u"}, "org")
	u, _ := url.Parse(link)
	state := u.Query().Get("state")
	if s.Callback(ctx, state, "") == nil {
		t.Fatal("cancel accepted")
	}
	if len(m.states["org"].Attempts) != 0 {
		t.Fatal("cancel was reusable")
	}
	link, _ = s.Connect(ctx, domain.Principal{UserID: "u"}, "org")
	u, _ = url.Parse(link)
	state = u.Query().Get("state")
	m.states["org"].Attempts[Hash(state)].Expires = time.Now().Add(-time.Second)
	if !errors.Is(s.Callback(ctx, state, "code"), ErrExpired) {
		t.Fatal("expired OAuth accepted")
	}
}
func TestWebhookAuthenticationIdentityAndReplay(t *testing.T) {
	s, m, _ := testService(t)
	st := fixture()
	st.Connections["c"].Workspace.ID = "workspace"
	st.Connections["c"].Agent.ID = "agent"
	m.states["org"] = st
	event := Event{Type: "AgentSessionEvent", Action: "created", OrganizationID: "workspace", AppUserID: "agent", OAuthClientID: "client", Timestamp: time.Now().UnixMilli(), PromptContext: "hello"}
	event.Session.ID = "task"
	event.Session.Issue = &struct {
		ID string `json:"id"`
	}{ID: "issue"}
	send := func(e Event, secret string) error {
		body, _ := json.Marshal(e)
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		return s.Webhook(context.Background(), body, hex.EncodeToString(mac.Sum(nil)))
	}
	if send(event, "bad") == nil {
		t.Fatal("bad signature accepted")
	}
	if err := send(event, "signing"); err != nil {
		t.Fatal(err)
	}
	send(event, "signing")
	if len(m.states["org"].Events) != 1 {
		t.Fatal("duplicate webhook")
	}
	event.Session.ID = "another"
	event.AppUserID = "other-agent"
	send(event, "signing")
	if len(m.states["org"].Events) != 1 {
		t.Fatal("wrong app identity accepted")
	}
	event.Timestamp = time.Now().Add(-2 * time.Minute).UnixMilli()
	if send(event, "signing") == nil {
		t.Fatal("stale webhook accepted")
	}
}

func TestRevocationPausesIntakeAndFencesRunningTasks(t *testing.T) {
	s, m, _ := testService(t)
	st := fixture()
	c := st.Connections["c"]
	c.Workspace.ID = "workspace"
	c.Agent.ID = "agent"
	c.Encrypted = []byte("encrypted")
	c.Nonce = []byte("nonce")
	p, _ := st.SaveProfile(profile("AO", ""), "owner")
	st.Enqueue("task", "c", "t", "", "created", "create", "hello")
	old := *st.Claim(p.ID, time.Now())
	m.states["org"] = st
	event := Event{Type: "OAuthApp", Action: "revoked", OrganizationID: "workspace", OAuthClientID: "client", Timestamp: time.Now().UnixMilli()}
	body, _ := json.Marshal(event)
	mac := hmac.New(sha256.New, []byte("signing"))
	mac.Write(body)
	if err := s.Webhook(context.Background(), body, hex.EncodeToString(mac.Sum(nil))); err != nil {
		t.Fatal(err)
	}
	st = m.states["org"]
	if !st.Connections["c"].Revoked || len(st.Connections["c"].Encrypted) != 0 || !st.Profiles[p.ID].Paused {
		t.Fatal("revocation retained credentials or intake")
	}
	if st.Check(p.ID, old, false, time.Now()) {
		t.Fatal("revocation did not fence active lease")
	}
	stop := st.Claim(p.ID, time.Now())
	if stop == nil || stop.Kind != "stop" {
		t.Fatal("revocation did not queue durable stop")
	}
}

func TestPendingFollowupsKeepOrder(t *testing.T) {
	s, m, _ := testService(t)
	st := fixture()
	c := st.Connections["c"]
	c.Workspace.ID = "workspace"
	c.Agent.ID = "agent"
	p, _ := st.SaveProfile(profile("AO", ""), "owner")
	st.Enqueue("task", "c", "t", "", "created", "create", "initial")
	m.states["org"] = st
	now := time.Now().UnixMilli()
	for _, item := range []struct {
		id, body string
		stamp    int64
	}{{"second", "second prompt", now}, {"first", "first prompt", now - 1}} {
		e := Event{Type: "AgentSessionEvent", Action: "prompted", OrganizationID: "workspace", OAuthClientID: "client", AppUserID: "agent", Timestamp: item.stamp}
		e.Session.ID = "task"
		e.Activity.ID = item.id
		e.Activity.Content.Body = item.body
		raw, _ := json.Marshal(e)
		mac := hmac.New(sha256.New, []byte("signing"))
		mac.Write(raw)
		if err := s.Webhook(context.Background(), raw, hex.EncodeToString(mac.Sum(nil))); err != nil {
			t.Fatal(err)
		}
	}
	st = m.states["org"]
	s.processEvent(context.Background(), "org", st)
	s.processEvent(context.Background(), "org", st)
	commands := st.Tasks["task"].Commands
	if len(commands) != 3 || commands[1].Body != "first prompt" || commands[2].Body != "second prompt" || st.Tasks["task"].ProfileID != p.ID {
		t.Fatal("followups reordered or rebound", commands)
	}
}
