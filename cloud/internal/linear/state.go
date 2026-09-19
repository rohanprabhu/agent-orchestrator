// Package linear owns hosted Linear installations and local-runner dispatch.
package linear

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aoagents/agent-orchestrator/cloud/internal/domain"
	"github.com/google/uuid"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

var ErrInvalid = errors.New("invalid Linear configuration")
var ErrConflict = errors.New("Linear profile name or routing scope is already in use")
var ErrExpired = errors.New("authorization expired; connect Linear again")

// Store serializes updates per tenant. Callback and worker callers must first
// resolve an opaque, verified route; interactive callers always supply a principal.
type Store interface {
	LinearUpdate(context.Context, *domain.Principal, string, func(*State) error) error
	LinearRoute(context.Context, string, string) (string, error)
	LinearOrganizations(context.Context) ([]string, error)
}
type State struct {
	Events      map[string]*PendingEvent `json:"events"`
	Attempts    map[string]*Attempt      `json:"attempts"`
	Connections map[string]*Connection   `json:"connections"`
	Profiles    map[string]*Profile      `json:"profiles"`
	Tasks       map[string]*Task         `json:"tasks"`
	Outbox      map[string]*Delivery     `json:"outbox"`
}

func (s *State) Init() {
	if s.Events == nil {
		s.Events = map[string]*PendingEvent{}
	}
	if s.Attempts == nil {
		s.Attempts = map[string]*Attempt{}
	}
	if s.Connections == nil {
		s.Connections = map[string]*Connection{}
	}
	if s.Profiles == nil {
		s.Profiles = map[string]*Profile{}
	}
	if s.Tasks == nil {
		s.Tasks = map[string]*Task{}
	}
	if s.Outbox == nil {
		s.Outbox = map[string]*Delivery{}
	}
}

type Attempt struct {
	Principal domain.Principal
	Verifier  string
	Expires   time.Time
}
type Resource struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type Connection struct {
	ID        string   `json:"id"`
	Workspace Resource `json:"workspace"`
	Agent     struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
	} `json:"agent"`
	Teams []Resource `json:"teams"`
	// Credentials are included in the encrypted persistence object only, never DTOs.
	Encrypted []byte    `json:"encrypted"`
	Nonce     []byte    `json:"nonce"`
	Error     string    `json:"error,omitempty"`
	Revoked   bool      `json:"revoked"`
	RetryAt   time.Time `json:"retryAt"`
}
type Profile struct {
	ID              string    `json:"id"`
	ConnectionID    string    `json:"connectionId"`
	Name            string    `json:"name"`
	TeamID          string    `json:"teamId"`
	LinearProjectID string    `json:"linearProjectId"`
	LocalProjectID  string    `json:"localProjectId"`
	RunnerID        string    `json:"runnerId"`
	Challenge       string    `json:"challenge"`
	Owner           string    `json:"owner"`
	Paused          bool      `json:"paused"`
	Disconnected    bool      `json:"disconnected"`
	Suspended       bool      `json:"suspended"`
	LastSeen        time.Time `json:"lastSeen"`
}
type Command struct {
	ID    string    `json:"id"`
	Task  string    `json:"task"`
	Kind  string    `json:"kind"`
	Body  string    `json:"body"`
	Lease string    `json:"lease"`
	Until time.Time `json:"until"`
	Done  bool      `json:"done"`
}
type Task struct {
	ProfileID    string
	ConnectionID string
	Stopped      bool
	Commands     []*Command
}
type Report struct {
	ID   string `json:"id"`
	Task string `json:"task"`
	Kind string `json:"kind"`
	Body string `json:"body"`
}
type Delivery struct {
	Report     Report
	ProviderID string
	Sent       bool
}

func Hash(s string) string    { v := sha256.Sum256([]byte(s)); return hex.EncodeToString(v[:]) }
func NameKey(s string) string { return cases.Fold().String(norm.NFKC.String(strings.TrimSpace(s))) }
func (s *State) SaveProfile(p Profile, owner string) (*Profile, error) {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" || utf8.RuneCountInString(p.Name) > 64 || p.LocalProjectID == "" || p.RunnerID == "" {
		return nil, ErrInvalid
	}
	c := s.Connections[p.ConnectionID]
	if c == nil {
		return nil, ErrInvalid
	}
	allowed := false
	for _, t := range c.Teams {
		if t.ID == p.TeamID {
			allowed = true
		}
	}
	if !allowed {
		return nil, ErrInvalid
	}
	// A retry after a lost create response must reuse its already-bound runner.
	if p.ID == "" {
		for _, v := range s.Profiles {
			if v.RunnerID == p.RunnerID || v.Challenge == p.Challenge {
				if !v.Disconnected && v.Owner == owner && v.RunnerID == p.RunnerID && v.Challenge == p.Challenge && v.ConnectionID == p.ConnectionID && v.TeamID == p.TeamID && v.LinearProjectID == p.LinearProjectID && v.LocalProjectID == p.LocalProjectID && v.Name == p.Name {
					return v, nil
				}
				return nil, ErrConflict
			}
		}
	}
	old := s.Profiles[p.ID]
	if p.ID != "" && (old == nil || old.Owner != owner || old.Disconnected) {
		return nil, ErrInvalid
	}
	for _, v := range s.Profiles {
		if v.ID != p.ID && !v.Disconnected && v.ConnectionID == p.ConnectionID && (NameKey(v.Name) == NameKey(p.Name) || (v.TeamID == p.TeamID && v.LinearProjectID == p.LinearProjectID)) {
			return nil, ErrConflict
		}
	}
	if old != nil {
		// Machine/project ownership is immutable: editing intake never moves active work.
		if p.LocalProjectID != old.LocalProjectID || p.RunnerID != old.RunnerID || p.ConnectionID != old.ConnectionID {
			return nil, ErrInvalid
		}
		p.Challenge = old.Challenge
		p.LastSeen = old.LastSeen
	} else {
		if len(p.Challenge) != 64 {
			return nil, ErrInvalid
		}
		if _, err := hex.DecodeString(p.Challenge); err != nil {
			return nil, ErrInvalid
		}
		p.ID = uuid.NewString()
		p.LastSeen = time.Time{}
	}
	p.Owner = owner
	p.Disconnected = false
	p.Suspended = false
	s.Profiles[p.ID] = &p
	return &p, nil
}
func (s *State) Enqueue(task, connection, team, project, id, kind, body string) {
	t := s.Tasks[task]
	if t == nil {
		if kind != "create" {
			return
		}
		var match *Profile
		for _, p := range s.Profiles {
			if p.ConnectionID != connection || p.TeamID != team || p.Disconnected {
				continue
			}
			if p.LinearProjectID == project && project != "" {
				match = p
				break
			}
			if p.LinearProjectID == "" {
				match = p
			}
		}
		if match == nil || match.Paused {
			s.Tasks[task] = &Task{ConnectionID: connection}
			s.Report(Report{ID: id + ":unrouted", Task: task, Kind: "error", Body: "Lenticular has no enabled profile for this team/project. Configure Linear in Lenticular Settings → Integrations, then delegate a new session."})
			return
		}
		t = &Task{ProfileID: match.ID, ConnectionID: connection}
		s.Tasks[task] = t
	}
	for _, c := range t.Commands {
		if c.ID == id {
			return
		}
	}
	if kind == "stop" {
		t.Stopped = true
		for _, c := range t.Commands {
			c.Done = true
			c.Lease = ""
		}
	} else if t.Stopped {
		return
	}
	t.Commands = append(t.Commands, &Command{ID: id, Task: task, Kind: kind, Body: body})
}
func (s *State) Report(r Report) {
	if s.Outbox[r.ID] == nil {
		s.Outbox[r.ID] = &Delivery{Report: r, ProviderID: uuid.NewString()}
	}
}
func (s *State) Claim(profile string, now time.Time) *Command {
	for _, t := range s.Tasks {
		if t.ProfileID != profile {
			continue
		}
		for _, c := range t.Commands {
			if c.Done {
				continue
			}
			if now.Before(c.Until) {
				break
			}
			c.Lease = uuid.NewString()
			c.Until = now.Add(5 * time.Minute)
			return c
		}
	}
	return nil
}
func (s *State) Check(profile string, c Command, ack bool, now time.Time) bool {
	t := s.Tasks[c.Task]
	if t == nil || t.ProfileID != profile {
		return false
	}
	for _, v := range t.Commands {
		if v.ID == c.ID && v.Lease == c.Lease && c.Lease != "" && !v.Done && now.Before(v.Until) {
			if ack {
				v.Done = true
			}
			return true
		}
	}
	return false
}
