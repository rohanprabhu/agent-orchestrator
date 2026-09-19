package linearintegration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunnerSecretPersistenceAndVerifiedActivation(t *testing.T) {
	var runner Runner
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Error("missing credential")
		}
		if r.URL.Path == workerPrefix+"/worker/status" {
			json.NewEncoder(w).Encode(map[string]string{"id": "profile", "runnerId": runner.ID, "localProjectId": "project"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m, err := New(ctx, dir, server.URL, filepath.Join(dir, "running.json"))
	if err != nil {
		t.Fatal(err)
	}
	runner, err = m.Prepare("project")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := m.Prepare("project")
	if runner.ID != again.ID {
		t.Fatal("setup retry changed identity")
	}
	raw, _ := json.Marshal(m.List())
	if strings.Contains(string(raw), "token") {
		t.Fatal("credential exposed")
	}
	if _, err = m.Activate(ctx, runner.ID, "wrong-profile"); err == nil {
		t.Fatal("unverified registration activated")
	}
	if _, err = m.Activate(ctx, runner.ID, "profile"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for m.List()[0].LastContact.IsZero() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if m.List()[0].LastContact.IsZero() {
		t.Fatal("worker did not poll hosted prefix")
	}
	m.Close()
	info, err := os.Stat(filepath.Join(dir, "integrations", "linear", "runners.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("credential file permissions", err)
	}
	restored, err := New(ctx, dir, server.URL, filepath.Join(dir, "running.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if restored.List()[0].ProfileID != "profile" {
		t.Fatal("profile not restored")
	}
	if err := restored.Disconnect(runner.ID); err != nil {
		t.Fatal(err)
	}
	changed, err := New(ctx, dir, "https://different.example", filepath.Join(dir, "running.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer changed.Close()
	if _, err := changed.Activate(ctx, runner.ID, "profile"); err == nil {
		t.Fatal("credential sent to a different service")
	}
}
