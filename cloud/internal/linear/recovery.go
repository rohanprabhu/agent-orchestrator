package linear

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// These fields and pagination arguments are from Linear's published GraphQL SDL.
// Recovery uses provider IDs also used by Webhook, never creates provider sessions.
const recoveryFields = `id status createdAt dismissedAt appUser { id } issue { id identifier title description team { id } project { id } delegate { id } state { type } } sourceComment { body }`
const recoveryActivities = `activities(first:100, after:$after, orderBy:createdAt) { nodes { id createdAt signal content { __typename ... on AgentActivityPromptContent { body } } } pageInfo { hasNextPage endCursor } }`

type recoveryPage struct {
	HasNextPage bool
	EndCursor   string
}
type recoveryActivity struct {
	ID        string
	CreatedAt time.Time
	Signal    string
	Content   struct {
		Type string `json:"__typename"`
		Body string
	}
}
type recoverySession struct {
	ID, Status  string
	CreatedAt   time.Time
	DismissedAt *time.Time
	AppUser     struct{ ID string }
	Issue       *struct {
		ID, Identifier, Title, Description string
		Team                               Resource
		Project, Delegate                  *Resource
		State                              struct{ Type string }
	}
	SourceComment *struct{ Body string }
	Activities    struct {
		Nodes    []recoveryActivity
		PageInfo recoveryPage
	}
}

func (s *Service) recoverySession(ctx context.Context, token, id string) (*recoverySession, error) {
	var result *recoverySession
	after := ""
	for page := 0; page < 100; page++ {
		var data struct{ AgentSession *recoverySession }
		if err := s.query(ctx, token, `query LenticularRecoverySession($id:String!,$after:String) { agentSession(id:$id) { `+recoveryFields+` `+recoveryActivities+` } }`, map[string]any{"id": id, "after": cursorValue(after)}, &data); err != nil {
			return nil, err
		}
		if data.AgentSession == nil || data.AgentSession.ID != id {
			return nil, errors.New("Linear session could not be read")
		}
		if result == nil {
			result = data.AgentSession
		} else {
			result.Activities.Nodes = append(result.Activities.Nodes, data.AgentSession.Activities.Nodes...)
		}
		next := data.AgentSession.Activities.PageInfo
		if !next.HasNextPage {
			sort.Slice(result.Activities.Nodes, func(i, j int) bool {
				a, b := result.Activities.Nodes[i], result.Activities.Nodes[j]
				if !a.CreatedAt.Equal(b.CreatedAt) {
					return a.CreatedAt.Before(b.CreatedAt)
				}
				return a.ID < b.ID
			})
			return result, nil
		}
		if next.EndCursor == "" || next.EndCursor == after {
			return nil, errors.New("Linear activity pagination did not advance")
		}
		after = next.EndCursor
	}
	return nil, errors.New("Linear activity history exceeds recovery limit; no work was queued")
}
func cursorValue(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// retryableTask also recognizes unbound tasks persisted by the original release.
func (st *State) retryableTask(id string) bool {
	t := st.Tasks[id]
	if t == nil || t.ProfileID != "" || t.Stopped {
		return false
	}
	if t.Retryable {
		return true
	}
	for key, d := range st.Outbox {
		if d.Report.Task == id && strings.HasSuffix(key, ":unrouted") {
			return true
		}
	}
	return false
}

func (st *State) recoverSession(c *Connection, remote *recoverySession, retryID string) {
	if remote == nil || remote.AppUser.ID != c.Agent.ID || remote.ID == "" {
		return
	}
	task := st.Tasks[remote.ID]
	if task != nil && (task.ConnectionID != c.ID || task.Stopped) {
		return
	}
	// Read the entire history before applying anything, so an offline stop wins
	// over every earlier prompt and cannot race the newly recovered create.
	stopID := ""
	for _, a := range remote.Activities.Nodes {
		if a.Signal == "stop" {
			stopID = remote.ID + ":" + a.ID
		}
	}
	unavailable := remote.DismissedAt != nil || remote.Issue == nil
	if remote.Issue != nil {
		issue := remote.Issue
		unavailable = unavailable || issue.Delegate == nil || issue.Delegate.ID != c.Agent.ID || issue.State.Type == "completed" || issue.State.Type == "canceled"
	}
	if stopID != "" || unavailable {
		if task == nil {
			st.Tasks[remote.ID] = &Task{ConnectionID: c.ID, Stopped: true}
		} else {
			if stopID == "" {
				stopID = remote.ID + ":recovery-cancelled"
			}
			st.Enqueue(remote.ID, c.ID, "", "", stopID, "stop", "")
		}
		if retryID != "" {
			st.Report(Report{ID: retryID + ":unavailable", Task: remote.ID, Kind: "error", Body: "This Linear session was stopped, dismissed, or is no longer actionable. Start a new session to request more work."})
		}
		return
	}
	if remote.Status == "complete" {
		return
	}
	if task == nil && remote.Status != "pending" && remote.Status != "active" && remote.Status != "stale" && remote.Status != "awaitingInput" {
		return
	}
	if task != nil && task.ProfileID == "" && !st.retryableTask(remote.ID) {
		return
	}
	if task == nil || task.ProfileID == "" {
		issue := remote.Issue
		project := ""
		if issue.Project != nil {
			project = issue.Project.ID
		}
		body := fmt.Sprintf("Linear issue %s: %s\n\n%s", issue.Identifier, issue.Title, issue.Description)
		if remote.SourceComment != nil {
			body += "\n\nOriginal request:\n" + remote.SourceComment.Body
		}
		if pending := st.Events[remote.ID+":created"]; pending != nil && pending.Event.PromptContext != "" {
			body = pending.Event.PromptContext
		}
		if task != nil && task.InitialPrompt != "" {
			body = task.InitialPrompt
		}
		st.Enqueue(remote.ID, c.ID, issue.Team.ID, project, remote.ID+":created", "create", body)
		task = st.Tasks[remote.ID]
		if task == nil || task.ProfileID == "" {
			if retryID != "" {
				st.Report(Report{ID: retryID + ":unrouted", Task: remote.ID, Kind: "error", Body: "Lenticular still has no enabled profile for this team/project. Configure or resume the profile in Settings → Integrations, then reply here to retry."})
			}
			return
		}
		for _, d := range st.Outbox {
			if d.Report.Task == remote.ID && strings.HasSuffix(d.Report.ID, ":unrouted") {
				d.Sent = true
			}
		}
		st.Report(Report{ID: remote.ID + ":created:queued", Task: remote.ID, Kind: "thought", Body: "Lenticular recovered this session and queued it for your local runner."})
	}
	for _, a := range remote.Activities.Nodes {
		if a.ID != "" && a.Content.Type == "AgentActivityPromptContent" {
			st.Enqueue(remote.ID, c.ID, "", "", remote.ID+":"+a.ID, "message", a.Content.Body)
		}
	}
}

// Reconcile processes one provider page per connection. The cursor survives a
// restart; failures retain it and retry. Network discovery runs outside tenant
// transactions so a slow provider cannot block webhook acceptance or workers.
func (s *Service) Reconcile(ctx context.Context, org string) {
	var ids []string
	_ = s.Store.LinearUpdate(ctx, nil, org, func(st *State) error {
		for id, c := range st.Connections {
			if !c.Revoked && !time.Now().Before(c.NextRecoveryAt) && !time.Now().Before(c.RetryAt) {
				for _, p := range st.Profiles {
					if p.ConnectionID == id && !p.Disconnected && !p.Suspended {
						ids = append(ids, id)
						break
					}
				}
			}
		}
		return nil
	})
	sort.Strings(ids)
	for _, id := range ids {
		s.reconcileConnection(ctx, org, id)
	}
}
func (s *Service) reconcileConnection(ctx context.Context, org, id string) {
	parentCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var token, after, agent string
	err := s.Store.LinearUpdate(ctx, nil, org, func(st *State) error {
		c := st.Connections[id]
		if c == nil || c.Revoked {
			return ErrInvalid
		}
		var e error
		token, e = s.access(ctx, org, c)
		after = c.RecoveryCursor
		agent = c.Agent.ID
		return e
	})
	if err != nil {
		s.recoveryFailed(parentCtx, org, id)
		return
	}
	var data struct {
		AgentSessions struct {
			Nodes []struct {
				ID      string
				AppUser struct{ ID string }
			}
			PageInfo recoveryPage
		}
	}
	err = s.query(ctx, token, `query LenticularRecoverySessions($after:String) { agentSessions(first:25,after:$after,orderBy:createdAt) { nodes { id appUser { id } } pageInfo { hasNextPage endCursor } } }`, map[string]any{"after": cursorValue(after)}, &data)
	if err != nil {
		s.recoveryFailed(parentCtx, org, id)
		return
	}
	for _, node := range data.AgentSessions.Nodes {
		if node.AppUser.ID != agent {
			continue
		}
		remote, e := s.recoverySession(ctx, token, node.ID)
		if e != nil {
			s.recoveryFailed(parentCtx, org, id)
			return
		}
		e = s.Store.LinearUpdate(ctx, nil, org, func(st *State) error {
			c := st.Connections[id]
			if c == nil || c.Revoked || c.Agent.ID != agent {
				return ErrInvalid
			}
			st.recoverSession(c, remote, "")
			return nil
		})
		if e != nil {
			return
		}
	}
	next := data.AgentSessions.PageInfo
	if next.HasNextPage && (next.EndCursor == "" || next.EndCursor == after) {
		s.recoveryFailed(parentCtx, org, id)
		return
	}
	_ = s.Store.LinearUpdate(ctx, nil, org, func(st *State) error {
		c := st.Connections[id]
		if c == nil || c.Revoked || c.RecoveryCursor != after {
			return nil
		}
		c.RecoveryCursor = ""
		c.NextRecoveryAt = time.Now().Add(time.Minute)
		if next.HasNextPage {
			c.RecoveryCursor = next.EndCursor
			c.NextRecoveryAt = time.Time{}
		} else {
			c.LastRecoveryAt = time.Now()
		}
		if strings.HasPrefix(c.Error, "Linear catch-up") {
			c.Error = ""
		}
		return nil
	})
}
func (s *Service) recoveryFailed(ctx context.Context, org, id string) {
	_ = s.Store.LinearUpdate(ctx, nil, org, func(st *State) error {
		if c := st.Connections[id]; c != nil {
			c.NextRecoveryAt = time.Now().Add(time.Minute)
			c.Error = "Linear catch-up could not complete; it will retry automatically."
		}
		return nil
	})
}
