package linear

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"
)

type Event struct {
	Type           string `json:"type"`
	Action         string `json:"action"`
	OrganizationID string `json:"organizationId"`
	AppUserID      string `json:"appUserId"`
	OAuthClientID  string `json:"oauthClientId"`
	Timestamp      int64  `json:"webhookTimestamp"`
	PromptContext  string `json:"promptContext"`
	Session        struct {
		ID    string `json:"id"`
		Issue *struct {
			ID string `json:"id"`
		} `json:"issue"`
	} `json:"agentSession"`
	Activity struct {
		ID      string `json:"id"`
		Signal  string `json:"signal"`
		Content struct {
			Body string `json:"body"`
		} `json:"content"`
	} `json:"agentActivity"`
}

func (s *Service) Webhook(ctx context.Context, body []byte, signature string) error {
	mac := hmac.New(sha256.New, []byte(s.Config.WebhookSecret))
	mac.Write(body)
	sig, err := hex.DecodeString(signature)
	if err != nil || !hmac.Equal(sig, mac.Sum(nil)) {
		return errors.New("invalid webhook signature")
	}
	var event Event
	if json.Unmarshal(body, &event) != nil || time.Since(time.UnixMilli(event.Timestamp)) > time.Minute || time.Until(time.UnixMilli(event.Timestamp)) > time.Minute {
		return ErrInvalid
	}
	if event.Type != "AgentSessionEvent" && !(event.Type == "OAuthApp" && event.Action == "revoked") {
		return nil
	}
	org, err := s.Store.LinearRoute(ctx, "workspace", event.OrganizationID)
	if err != nil {
		return nil
	}
	return s.Store.LinearUpdate(ctx, nil, org, func(st *State) error {
		if event.Type == "OAuthApp" {
			if event.OAuthClientID != s.Config.ClientID {
				return nil
			}
			for id, c := range st.Connections {
				if c.Workspace.ID != event.OrganizationID {
					continue
				}
				c.Revoked = true
				c.Error = "Linear installation was removed; reconnect the workspace"
				c.Encrypted = nil
				c.Nonce = nil
				for _, p := range st.Profiles {
					if p.ConnectionID == id {
						p.Paused = true
					}
				}
				for task, t := range st.Tasks {
					if t.ConnectionID == id && t.ProfileID != "" {
						st.Enqueue(task, id, "", "", task+":revoked", "stop", "")
					}
				}
				for key, e := range st.Events {
					if e.ConnectionID == id {
						delete(st.Events, key)
					}
				}
			}
			return nil
		}

		var connection string
		for id, c := range st.Connections {
			if !c.Revoked && c.Workspace.ID == event.OrganizationID && c.Agent.ID == event.AppUserID && event.OAuthClientID == s.Config.ClientID {
				connection = id
			}
		}
		if connection == "" || event.Session.ID == "" {
			return nil
		}
		kind, id, body := "", event.Session.ID+":created", event.PromptContext
		switch event.Action {
		case "created":
			kind = "create"
		case "prompted":
			if event.Activity.ID == "" {
				return nil
			}
			id = event.Session.ID + ":" + event.Activity.ID
			body = event.Activity.Content.Body
			kind = "message"
			if event.Activity.Signal == "stop" {
				kind = "stop"
			}
		default:
			return nil
		}
		if st.Events[id] != nil {
			return nil
		}
		if task := st.Tasks[event.Session.ID]; task != nil && kind != "message" {
			// Stop supersedes a lease immediately; other follow-ups preserve order.
			st.Enqueue(event.Session.ID, connection, "", "", id, kind, body)
			return nil
		}
		if kind == "stop" {
			st.Tasks[event.Session.ID] = &Task{ConnectionID: connection, Stopped: true}
			return nil
		}
		if kind == "create" && event.Session.Issue == nil {
			st.Tasks[event.Session.ID] = &Task{ConnectionID: connection}
			st.Report(Report{ID: id + ":unsupported", Task: event.Session.ID, Kind: "error", Body: "This Lenticular integration accepts delegated issues. Please delegate an issue from a configured team."})
		} else {
			st.Events[id] = &PendingEvent{ConnectionID: connection, Event: event}
		}

		return nil
	})
}

type PendingEvent struct {
	ConnectionID string
	Event        Event
}

func (s *Service) processEvent(ctx context.Context, org string, st *State) bool {
	ids := make([]string, 0, len(st.Events))
	for id := range st.Events {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := st.Events[ids[i]].Event, st.Events[ids[j]].Event
		if a.Timestamp != b.Timestamp {
			return a.Timestamp < b.Timestamp
		}
		return ids[i] < ids[j]
	})
	for _, id := range ids {
		pending := st.Events[id]
		e := pending.Event
		c := st.Connections[pending.ConnectionID]
		if c == nil || c.Revoked {
			delete(st.Events, id)
			return true
		}
		if e.Action == "prompted" {
			if st.Tasks[e.Session.ID] != nil {
				st.Enqueue(e.Session.ID, c.ID, "", "", id, "message", e.Activity.Content.Body)
				delete(st.Events, id)
				return true
			}
			if time.Since(time.UnixMilli(e.Timestamp)) > 24*time.Hour {
				delete(st.Events, id)
			}
			continue
		}
		if st.Tasks[e.Session.ID] != nil {
			delete(st.Events, id)
			return true
		}
		if time.Now().Before(c.RetryAt) {
			continue
		}
		token, err := s.access(ctx, org, c)
		if err != nil {
			c.Error = err.Error()
			c.RetryAt = time.Now().Add(time.Minute)
			return true
		}
		var d struct {
			Issue struct {
				Team    Resource
				Project *Resource
			}
		}
		if err = s.query(ctx, token, `query($id:String!) { issue(id:$id) { team { id name } project { id name } } }`, map[string]string{"id": e.Session.Issue.ID}, &d); err != nil {
			c.Error = err.Error()
			c.RetryAt = time.Now().Add(time.Minute)
			return true
		}
		project := ""
		if d.Issue.Project != nil {
			project = d.Issue.Project.ID
		}
		st.Enqueue(e.Session.ID, c.ID, d.Issue.Team.ID, project, id, "create", e.PromptContext)
		if t := st.Tasks[e.Session.ID]; t != nil && t.ProfileID != "" {
			st.Report(Report{ID: id + ":queued", Task: e.Session.ID, Kind: "thought", Body: "Lenticular queued this request for your local runner. It will start when that machine is available."})
		}
		delete(st.Events, id)
		return true
	}
	return false
}
