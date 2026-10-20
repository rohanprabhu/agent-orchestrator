package linear

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func recoverable(t *testing.T) *recoverySession {
	t.Helper()
	var r recoverySession
	if err := json.Unmarshal([]byte(`{"id":"session","status":"active","appUser":{"id":"agent"},"issue":{"id":"issue","identifier":"LEKE-1","title":"Build planes","description":"Original instructions","team":{"id":"t"},"delegate":{"id":"agent"},"state":{"type":"started"}},"activities":{"nodes":[{"id":"reply","createdAt":"2026-09-20T01:00:00Z","content":{"__typename":"AgentActivityPromptContent","body":"try again"}}]}}`), &r); err != nil {
		t.Fatal(err)
	}
	return &r
}
func recoveryFixture(t *testing.T) (*State, *Connection) {
	t.Helper()
	st := fixture()
	c := st.Connections["c"]
	c.Agent.ID = "agent"
	return st, c
}
func TestRecoveryMissedCreateAndFollowupAreIdempotent(t *testing.T) {
	st, c := recoveryFixture(t)
	p, err := st.SaveProfile(profile("runner", ""), "owner")
	if err != nil {
		t.Fatal(err)
	}
	r := recoverable(t)
	st.recoverSession(c, r, "")
	st.recoverSession(c, r, "")
	st.Enqueue(r.ID, c.ID, "t", "", r.ID+":created", "create", "webhook duplicate")
	st.Enqueue(r.ID, c.ID, "", "", r.ID+":reply", "message", "webhook duplicate")
	task := st.Tasks[r.ID]
	if task.ProfileID != p.ID || len(task.Commands) != 2 {
		t.Fatalf("duplicate or missing work: %+v", task)
	}
	if !strings.Contains(task.Commands[0].Body, "Original instructions") || task.Commands[1].Body != "try again" {
		t.Fatal("lost request context or followup")
	}
}
func TestRecoveryMissingProfileRetry(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "legacy"}[legacy], func(t *testing.T) {
			st, c := recoveryFixture(t)
			r := recoverable(t)
			st.Enqueue(r.ID, c.ID, "t", "", r.ID+":created", "create", "Exact original webhook context")
			if legacy {
				st.Tasks[r.ID].Retryable = false
				st.Tasks[r.ID].InitialPrompt = ""
				st.Tasks[r.ID].Commands = []*Command{{ID: "orphan", Kind: "message"}}
			}
			st.recoverSession(c, r, r.ID+":reply")
			if len(st.Tasks[r.ID].Commands) != 0 || st.Outbox[r.ID+":reply:unrouted"] == nil {
				t.Fatal("retry without profile hung or queued orphan")
			}
			_, err := st.SaveProfile(profile("runner", ""), "owner")
			if err != nil {
				t.Fatal(err)
			}
			st.recoverSession(c, r, r.ID+":reply")
			task := st.Tasks[r.ID]
			if task.ProfileID == "" || len(task.Commands) != 2 {
				t.Fatalf("retry failed: %+v", task)
			}
			if !legacy && task.Commands[0].Body != "Exact original webhook context" {
				t.Fatal("lost original context")
			}
			if !st.Outbox[r.ID+":reply:unrouted"].Sent {
				t.Fatal("stale rejection will be delivered after success")
			}
		})
	}
}
func TestRecoveryCancellationFencesLateCreate(t *testing.T) {
	cases := []string{"stop", "dismissed", "completed", "undelegated", "different-agent"}
	for _, kind := range cases {
		t.Run(kind, func(t *testing.T) {
			st, c := recoveryFixture(t)
			st.SaveProfile(profile("runner", ""), "owner")
			r := recoverable(t)
			switch kind {
			case "stop":
				r.Activities.Nodes[0].Signal = "stop"
			case "dismissed":
				now := time.Now()
				r.DismissedAt = &now
			case "completed":
				r.Issue.State.Type = "completed"
			case "undelegated":
				r.Issue.Delegate = nil
			case "different-agent":
				r.Issue.Delegate.ID = "other"
			}
			st.recoverSession(c, r, "retry")
			st.Enqueue(r.ID, c.ID, "t", "", r.ID+":created", "create", "late webhook")
			task := st.Tasks[r.ID]
			if !task.Stopped || len(task.Commands) != 0 {
				t.Fatal("late create resurrected canceled task")
			}
			if st.Outbox["retry:unavailable"] == nil {
				t.Fatal("retry left without feedback")
			}
		})
	}
}
func TestRecoveryDoesNotBypassScopeOrSessionOwnership(t *testing.T) {
	for _, kind := range []string{"paused", "suspended", "wrong-team", "complete", "other-app"} {
		t.Run(kind, func(t *testing.T) {
			st, c := recoveryFixture(t)
			p, _ := st.SaveProfile(profile("runner", ""), "owner")
			r := recoverable(t)
			switch kind {
			case "paused":
				p.Paused = true
			case "suspended":
				p.Suspended = true
			case "wrong-team":
				r.Issue.Team.ID = "other"
			case "complete":
				r.Status = "complete"
			case "other-app":
				r.AppUser.ID = "other"
			}
			st.recoverSession(c, r, "")
			if task := st.Tasks[r.ID]; task != nil && len(task.Commands) > 0 {
				t.Fatal("ineligible session started")
			}
		})
	}
}
func TestRecoveryReadsAllPagesBeforeApplyingStop(t *testing.T) {
	svc, _, _ := testService(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		remote := recoverable(t)
		if calls == 1 {
			remote.Activities.PageInfo = recoveryPage{HasNextPage: true, EndCursor: "next"}
		} else {
			remote.Activities.Nodes[0].ID = "stop"
			remote.Activities.Nodes[0].Signal = "stop"
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"agentSession": remote}})
	}))
	defer server.Close()
	svc.GraphQLURL = server.URL
	remote, err := svc.recoverySession(context.Background(), "token", "session")
	if err != nil {
		t.Fatal(err)
	}
	st, c := recoveryFixture(t)
	st.SaveProfile(profile("runner", ""), "owner")
	st.recoverSession(c, remote, "")
	if calls != 2 || !st.Tasks["session"].Stopped || len(st.Tasks["session"].Commands) != 0 {
		t.Fatal("partial history started work before stop")
	}
}
func TestRecoveryRejectsIncompleteHistory(t *testing.T) {
	svc, _, _ := testService(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remote := recoverable(t)
		remote.Activities.PageInfo = recoveryPage{HasNextPage: true, EndCursor: "same"}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"agentSession": remote}})
	}))
	defer server.Close()
	svc.GraphQLURL = server.URL
	remote, err := svc.recoverySession(context.Background(), "token", "session")
	if err == nil || remote != nil {
		t.Fatal("incomplete history accepted")
	}
}
func TestReconcileCursorAndRestartDeduplication(t *testing.T) {
	svc, m, _ := testService(t)
	st, c := recoveryFixture(t)
	st.SaveProfile(profile("runner", ""), "owner")
	if err := svc.saveToken("org", c, credential{Access: "access", Expires: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	m.states["org"] = st
	fail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query     string
			Variables map[string]any
		}
		json.NewDecoder(r.Body).Decode(&request)
		if fail {
			w.WriteHeader(503)
			return
		}
		if strings.Contains(request.Query, "LenticularRecoverySessions(") {
			page := recoveryPage{}
			if request.Variables["after"] == nil {
				page = recoveryPage{HasNextPage: true, EndCursor: "page2"}
			}
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"agentSessions": map[string]any{"nodes": []any{map[string]any{"id": "session", "appUser": map[string]string{"id": "agent"}}}, "pageInfo": page}}})
		} else {
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"agentSession": recoverable(t)}})
		}
	}))
	defer server.Close()
	svc.GraphQLURL = server.URL
	svc.Reconcile(context.Background(), "org")
	if m.states["org"].Connections["c"].RecoveryCursor != "page2" {
		t.Fatal("cursor not persisted")
	}
	fail = true
	svc.Reconcile(context.Background(), "org")
	if m.states["org"].Connections["c"].RecoveryCursor != "page2" {
		t.Fatal("failure lost cursor")
	}
	fail = false
	m.states["org"].Connections["c"].NextRecoveryAt = time.Time{}
	svc.Reconcile(context.Background(), "org")
	state := m.states["org"]
	if state.Connections["c"].RecoveryCursor != "" || state.Connections["c"].LastRecoveryAt.IsZero() || len(state.Tasks["session"].Commands) != 2 {
		t.Fatal("resumed scan lost or duplicated work")
	}
}
func TestPromptedEventRecoversRejectedSession(t *testing.T) {
	svc, m, _ := testService(t)
	st, c := recoveryFixture(t)
	st.Enqueue("session", "c", "t", "", "session:created", "create", "original")
	st.SaveProfile(profile("runner", ""), "owner")
	if err := svc.saveToken("org", c, credential{Access: "access", Expires: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	event := Event{Action: "prompted"}
	event.Session.ID = "session"
	event.Activity.ID = "reply"
	event.Activity.Content.Body = "try again"
	st.Events["session:reply"] = &PendingEvent{ConnectionID: "c", Event: event}
	m.states["org"] = st
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"agentSession": recoverable(t)}})
	}))
	defer server.Close()
	svc.GraphQLURL = server.URL
	if !svc.processEvent(context.Background(), "org", st) || len(st.Events) != 0 || len(st.Tasks["session"].Commands) != 2 || st.Tasks["session"].Commands[0].Body != "original" {
		t.Fatal("prompt did not recover rejected session")
	}
}

// A paused profile stops new intake but must not lose replies for its bound tasks.
func TestRecoveryPausedProfileKeepsExistingConversation(t *testing.T) {
	st, c := recoveryFixture(t)
	p, _ := st.SaveProfile(profile("runner", ""), "owner")
	st.Enqueue("session", "c", "t", "", "session:created", "create", "original")
	p.Paused = true
	st.recoverSession(c, recoverable(t), "")
	if len(st.Tasks["session"].Commands) != 2 {
		t.Fatal("paused intake lost followup for existing task")
	}
}
