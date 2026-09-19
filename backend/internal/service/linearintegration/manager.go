// Package linearintegration manages outbound Linear workers for this daemon.
package linearintegration

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/linearbridge"
)

const workerPrefix = "/api/cloud/v1/linear"

// ErrInvalid indicates an unknown or inconsistent runner registration.
var ErrInvalid = errors.New("invalid Linear runner configuration")

// Runner is the public, credential-free view of a local registration.
type Runner struct {
	ID          string    `json:"id"`
	ProfileID   string    `json:"profileId"`
	ProjectID   string    `json:"projectId"`
	Challenge   string    `json:"challenge"`
	Active      bool      `json:"active"`
	LastContact time.Time `json:"lastContact"`
	Error       string    `json:"error,omitempty"`
}
type registration struct {
	Origin string `json:"origin"`
	Runner Runner `json:"runner"`
	Token  string `json:"token"`
}

// Manager owns persisted registrations and outbound worker lifecycles.
type Manager struct {
	mu                   sync.Mutex
	ctx                  context.Context
	dir, origin, runFile string
	entries              map[string]*registration
	cancels              map[string]context.CancelFunc
	wg                   sync.WaitGroup
}

// New restores registered workers under the daemon lifetime context.
func New(ctx context.Context, dataDir, origin, runFile string) (*Manager, error) {
	m := &Manager{ctx: ctx, dir: filepath.Join(dataDir, "integrations", "linear"), origin: strings.TrimRight(origin, "/"), runFile: runFile, entries: map[string]*registration{}, cancels: map[string]context.CancelFunc{}}
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(m.dir, "runners.json"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(raw, &m.entries); err != nil {
			return nil, err
		}
	}
	if m.entries == nil {
		return nil, ErrInvalid
	}
	for id, e := range m.entries {
		if _, err := uuid.Parse(id); err != nil || e == nil || e.Runner.ID != id || len(e.Token) < 32 {
			return nil, ErrInvalid
		}
	}
	if origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" || (u.Scheme != "https" && (u.Scheme != "http" || u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost")) {
			return nil, ErrInvalid
		}
	}
	for _, e := range m.entries {
		if e.Runner.Active && origin != "" && e.Origin == m.origin {
			m.start(e)
		}
	}
	return m, nil
}

// Close stops and joins all outbound workers.
func (m *Manager) Close() {
	m.mu.Lock()
	for _, cancel := range m.cancels {
		cancel()
	}
	m.mu.Unlock()
	m.wg.Wait()
}

// List returns public runner snapshots without their bearer credentials.
func (m *Manager) List() []Runner {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Runner, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, e.Runner)
	}
	return out
}
func (m *Manager) save() error {
	raw, err := json.Marshal(m.entries)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(m.dir, ".runners-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), filepath.Join(m.dir, "runners.json"))
}

// Prepare durably reserves a credential for a local project.
func (m *Manager) Prepare(project string) (Runner, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.origin == "" || project == "" {
		return Runner{}, ErrInvalid
	}
	// Reopening an interrupted setup reuses its secret; no second runner is created.
	for _, e := range m.entries {
		if e.Origin == m.origin && e.Runner.ProjectID == project && !e.Runner.Active && e.Runner.ProfileID == "" {
			return e.Runner, nil
		}
	}
	if len(m.entries) >= 100 {
		return Runner{}, ErrInvalid
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return Runner{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(b[:])
	hash := sha256.Sum256([]byte(token))
	r := Runner{ID: uuid.NewString(), ProjectID: project, Challenge: hex.EncodeToString(hash[:])}
	m.entries[r.ID] = &registration{Origin: m.origin, Runner: r, Token: token}
	if err := m.save(); err != nil {
		delete(m.entries, r.ID)
		return Runner{}, err
	}
	return r, nil
}

// Activate verifies the hosted binding before starting its local worker.
func (m *Manager) Activate(ctx context.Context, id, profile string) (Runner, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[id]
	if e == nil || e.Origin != m.origin || profile == "" || (e.Runner.ProfileID != "" && e.Runner.ProfileID != profile) {
		return Runner{}, ErrInvalid
	}
	// Confirm the cloud registration before launching anything locally. The
	// credential stays in this process and the mode-0o600 daemon state file.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.origin+workerPrefix+"/worker/status", http.NoBody)
	if err != nil {
		return Runner{}, err
	}
	req.Header.Set("Authorization", "Bearer "+e.Token)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return Runner{}, errors.New("could not reach Linear integration service")
	}
	defer func() { _ = resp.Body.Close() }()
	var remote struct{ ID, RunnerID, LocalProjectID string }
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&remote) != nil || remote.ID != profile || remote.RunnerID != id || remote.LocalProjectID != e.Runner.ProjectID {
		return Runner{}, errors.New("cloud runner registration could not be verified")
	}
	old := e.Runner
	e.Runner.ProfileID = profile
	e.Runner.Active = true
	if err = m.save(); err != nil {
		e.Runner = old
		return Runner{}, err
	}
	m.start(e)
	return e.Runner, nil
}

// Disconnect disables local polling without terminating existing AO sessions.
func (m *Manager) Disconnect(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[id]
	if e == nil {
		return ErrInvalid
	}
	old := e.Runner
	e.Runner.Active = false
	if err := m.save(); err != nil {
		e.Runner = old
		return err
	}
	if cancel := m.cancels[id]; cancel != nil {
		cancel()
		delete(m.cancels, id)
	}
	return nil
}
func (m *Manager) start(e *registration) {
	if m.cancels[e.Runner.ID] != nil {
		return
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.cancels[e.Runner.ID] = cancel
	m.wg.Add(1)
	id, token, project := e.Runner.ID, e.Token, e.Runner.ProjectID
	go func() {
		defer m.wg.Done()
		defer cancel()
		store, err := linearbridge.Open(filepath.Join(m.dir, id+".db"))
		if err != nil {
			m.update(id, err)
			return
		}
		defer func() { _ = store.Close() }()
		worker := &linearbridge.Worker{Store: store, Coordinator: m.origin, APIPrefix: workerPrefix, Token: token, ProjectID: project, RunFile: m.runFile, Client: &http.Client{Timeout: 30 * time.Second}}
		if err := worker.Validate(ctx); err != nil {
			m.update(id, err)
			return
		}
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			m.update(id, worker.Step(ctx))
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
}
func (m *Manager) update(id string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[id]
	if e == nil {
		return
	}
	if err != nil {
		e.Runner.Error = "Runner could not complete a poll. Check this machine’s connection and Linear authorization."
	} else {
		e.Runner.Error = ""
		e.Runner.LastContact = time.Now()
	}
}
