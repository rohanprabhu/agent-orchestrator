package linearbridge

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func testServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s := &Server{Store: testStore(t), WebhookSecret: "secret", WorkerToken: strings.Repeat("x", 32), OrganizationID: "org", TeamID: "team"}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	return s, h
}
func event(action, id, body string) map[string]any {
	return map[string]any{
		"type": "AgentSessionEvent", "action": action, "organizationId": "org", "webhookTimestamp": time.Now().UnixMilli(), "promptContext": "Please fix the test",
		"agentSession":  map[string]any{"id": "task", "issue": map[string]any{"id": "issue", "team": map[string]string{"id": "team"}}},
		"agentActivity": map[string]any{"id": id, "content": map[string]string{"type": "prompt", "body": body}},
	}
}
func webhookRequest(h http.Handler, payload map[string]any, secret string) int {
	raw, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/linear/webhook", strings.NewReader(string(raw)))
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(raw)
	req.Header.Set("Linear-Signature", hex.EncodeToString(mac.Sum(nil)))
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	return res.Code
}
func TestWebhookAuthenticationScopeAndReplay(t *testing.T) {
	s, h := testServer(t)
	for _, tc := range []struct {
		name   string
		edit   func(map[string]any)
		secret string
		want   int
	}{
		{"signature", func(map[string]any) {}, "wrong", 401},
		{"stale", func(v map[string]any) { v["webhookTimestamp"] = time.Now().Add(-2 * time.Minute).UnixMilli() }, "secret", 401},
		{"future", func(v map[string]any) { v["webhookTimestamp"] = time.Now().Add(2 * time.Minute).UnixMilli() }, "secret", 401},
		{"workspace", func(v map[string]any) { v["organizationId"] = "other" }, "secret", 403},
		{"team", func(v map[string]any) {
			v["agentSession"].(map[string]any)["issue"].(map[string]any)["team"] = map[string]string{"id": "other"}
		}, "secret", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := event("created", "", "")
			tc.edit(e)
			if got := webhookRequest(h, e, tc.secret); got != tc.want {
				t.Fatalf("got %d want %d", got, tc.want)
			}
		})
	}
	for range 2 {
		if got := webhookRequest(h, event("created", "", ""), "secret"); got != 200 {
			t.Fatal(got)
		}
	}
	var count int
	_ = s.Store.db.QueryRow(`SELECT count(*) FROM commands`).Scan(&count)
	if count != 1 {
		t.Fatalf("duplicate created %d commands", count)
	}
	req := httptest.NewRequest(http.MethodGet, "/worker/commands", nil)
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != 401 {
		t.Fatal(res.Code)
	}
}
func TestQueueSurvivesRestartAndFencesLease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coordinator.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err = s.enqueue(ctx, Command{ID: "one", Task: "task", Kind: "create", Body: "start"}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first, err := s.claim(ctx)
	if err != nil || first == nil {
		t.Fatalf("%v %v", first, err)
	}
	if got, err := s.claim(ctx); err != nil || got != nil {
		t.Fatalf("claimed leased command: %v %v", got, err)
	}
	_, _ = s.db.Exec(`UPDATE commands SET until_ms=0`)
	second, err := s.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ack(ctx, *first); err == nil {
		t.Fatal("stale lease acknowledged")
	}
	if err = s.ack(ctx, *second); err != nil {
		t.Fatal(err)
	}
}
func TestStopSupersedesQueuedAndClaimedWork(t *testing.T) {
	s, h := testServer(t)
	ctx := context.Background()
	_ = webhookRequest(h, event("created", "", ""), "secret")
	first, err := s.Store.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = webhookRequest(h, event("prompted", "m1", "follow up"), "secret")
	stop := event("prompted", "stop", "")
	stop["agentActivity"].(map[string]any)["signal"] = "stop"
	if got := webhookRequest(h, stop, "secret"); got != 200 {
		t.Fatal(got)
	}
	if err = s.Store.ack(ctx, *first); err == nil {
		t.Fatal("stop did not fence create")
	}
	next, err := s.Store.claim(ctx)
	if err != nil || next.Kind != "stop" {
		t.Fatalf("%+v %v", next, err)
	}
	if err = s.Store.ack(ctx, *next); err != nil {
		t.Fatal(err)
	}
	_ = webhookRequest(h, event("prompted", "m2", "late prompt"), "secret")
	if next, err = s.Store.claim(ctx); err != nil || next != nil {
		t.Fatalf("stopped task received work: %+v %v", next, err)
	}
}
func TestConcurrentClaimsHaveOneWinner(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_ = s.enqueue(ctx, Command{ID: "one", Task: "task", Kind: "create", Body: "start"})
	var wg sync.WaitGroup
	results := make(chan *Command, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := s.claim(ctx)
			if err != nil {
				t.Error(err)
			}
			results <- c
		}()
	}
	wg.Wait()
	close(results)
	count := 0
	for c := range results {
		if c != nil {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("%d claimants", count)
	}
}
func TestWorkerEndToEndThroughDaemonHTTP(t *testing.T) {
	bridge, handler := testServer(t)
	coordinator := httptest.NewServer(handler)
	defer coordinator.Close()
	creates, kills := 0, 0
	messages := map[string]string{}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/projects/project":
			writeJSON(w, map[string]any{"project": map[string]string{"id": "project"}})
		case "POST /api/v1/sessions":
			creates++
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["mode"] != "chat" || body["projectId"] != "project" || body["prompt"] != "" {
				t.Errorf("unexpected create: %+v", body)
			}
			writeJSON(w, map[string]any{"session": map[string]string{"id": "local"}})
		case "POST /api/v1/sessions/local/conversation/messages":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			messages[body["clientMessageId"]] = body["text"]
			writeJSON(w, map[string]any{"state": "running"})
		case "GET /api/v1/sessions/local/conversation":
			writeJSON(w, map[string]any{"turns": []any{map[string]string{"id": "turn", "state": "completed"}}, "messages": []any{map[string]any{"turnId": "turn", "role": "assistant", "text": "Fixed. PR is ready.", "streaming": false}}})
		case "GET /api/v1/sessions/local":
			writeJSON(w, map[string]any{"session": map[string]any{"id": "local", "projectId": "project", "mode": "chat", "isTerminated": kills > 0}})
		case "POST /api/v1/sessions/local/kill":
			kills++
			writeJSON(w, map[string]bool{"ok": true})
		default:
			t.Errorf("unexpected daemon route: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer daemon.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(daemon.URL, "http://"))
	p, _ := strconv.Atoi(port)
	runPath := filepath.Join(t.TempDir(), "running.json")
	if err := runfile.Write(runPath, runfile.Info{Port: p}); err != nil {
		t.Fatal(err)
	}
	worker := &Worker{Store: testStore(t), Coordinator: coordinator.URL, Token: bridge.WorkerToken, ProjectID: "project", RunFile: runPath, Client: coordinator.Client()}
	ctx := context.Background()
	if err := worker.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	_ = webhookRequest(handler, event("created", "", ""), "secret")
	if err := worker.Step(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		_ = webhookRequest(handler, event("prompted", "followup", "Add tests too"), "secret")
	}
	if err := worker.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if err := worker.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if creates != 1 || len(messages) != 2 {
		t.Fatalf("creates=%d messages=%v", creates, messages)
	}
	var count int
	_ = bridge.Store.db.QueryRow(`SELECT count(*) FROM outbox WHERE kind='response' AND body LIKE 'Fixed%'`).Scan(&count)
	if count != 1 {
		t.Fatalf("final response repeated %d times", count)
	}
	stop := event("prompted", "stop", "")
	stop["agentActivity"].(map[string]any)["signal"] = "stop"
	_ = webhookRequest(handler, stop, "secret")
	if err := worker.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if kills != 1 {
		t.Fatalf("kills=%d", kills)
	}
}
func TestUncertainCreateNeverCreatesAgain(t *testing.T) {
	worker := &Worker{Store: testStore(t)}
	ctx := context.Background()
	_, _ = worker.Store.db.Exec(`INSERT INTO bindings(task,uncertain) VALUES('task',1)`)
	// No HTTP client/run file: trying to spawn or send here would fail/panic.
	if err := worker.execute(ctx, Command{ID: "retry", Task: "task", Kind: "create"}); err != nil {
		t.Fatal(err)
	}
	var count int
	_ = worker.Store.db.QueryRow(`SELECT count(*) FROM reports WHERE kind='error'`).Scan(&count)
	if count != 1 {
		t.Fatal(count)
	}
}
func TestLinearGraphQLErrorsAreRetried(t *testing.T) {
	calls := 0
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer app-token" {
			t.Error("missing app token")
		}
		if calls == 1 {
			fmt.Fprint(w, `{"errors":[{"message":"rate limited"}]}`)
		} else {
			fmt.Fprint(w, `{"data":{"agentActivityCreate":{"success":true}}}`)
		}
	}))
	defer endpoint.Close()
	s, _ := testServer(t)
	s.Linear = &Linear{Token: "app-token", Client: endpoint.Client(), Endpoint: endpoint.URL}
	ctx := context.Background()
	_ = s.Store.enqueue(ctx, Command{ID: "one", Task: "task", Kind: "create", Body: "start"})
	if err := s.publish(ctx); err == nil {
		t.Fatal("GraphQL errors treated as success")
	}
	if err := s.publish(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.publish(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal(calls)
	}
}

func TestWorkerRejectsInsecureOriginAndChangedScope(t *testing.T) {
	w := &Worker{Store: testStore(t), Coordinator: "http://example.com", Token: strings.Repeat("x", 32), ProjectID: "project", RunFile: "running.json"}
	ctx := context.Background()
	if err := w.Validate(ctx); err == nil {
		t.Fatal("accepted remote plaintext coordinator")
	}
	w.Coordinator = "https://example.com"
	if err := w.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	w.ProjectID = "other"
	if err := w.Validate(ctx); err == nil {
		t.Fatal("accepted changed project with existing journal")
	}
}

func TestStopDuringCreatePreventsInitialPrompt(t *testing.T) {
	bridge, handler := testServer(t)
	coordinator := httptest.NewServer(handler)
	defer coordinator.Close()
	sends, kills := 0, 0
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/projects/project":
			writeJSON(w, map[string]any{})
		case "POST /api/v1/sessions":
			stop := event("prompted", "stop", "")
			stop["agentActivity"].(map[string]any)["signal"] = "stop"
			if code := webhookRequest(handler, stop, "secret"); code != 200 {
				t.Errorf("stop HTTP %d", code)
			}
			writeJSON(w, map[string]any{"session": map[string]string{"id": "local"}})
		case "POST /api/v1/sessions/local/conversation/messages":
			sends++
			writeJSON(w, map[string]any{})
		case "GET /api/v1/sessions/local":
			writeJSON(w, map[string]any{"session": map[string]bool{"isTerminated": false}})
		case "POST /api/v1/sessions/local/kill":
			kills++
			writeJSON(w, map[string]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer daemon.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(daemon.URL, "http://"))
	p, _ := strconv.Atoi(port)
	runPath := filepath.Join(t.TempDir(), "running.json")
	if err := runfile.Write(runPath, runfile.Info{Port: p}); err != nil {
		t.Fatal(err)
	}
	worker := &Worker{Store: testStore(t), Coordinator: coordinator.URL, Token: bridge.WorkerToken, ProjectID: "project", RunFile: runPath, Client: coordinator.Client()}
	_ = webhookRequest(handler, event("created", "", ""), "secret")
	if err := worker.Step(context.Background()); err == nil {
		t.Fatal("superseded create was delivered")
	}
	if err := worker.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sends != 0 || kills != 1 {
		t.Fatalf("sent=%d killed=%d", sends, kills)
	}
}
