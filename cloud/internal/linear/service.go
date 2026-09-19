package linear

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/cloud/internal/domain"
	"github.com/aoagents/agent-orchestrator/cloud/internal/secrets"
	"github.com/google/uuid"
)

type Config struct{ ClientID, ClientSecret, RedirectURL, WebhookSecret string }
type Service struct {
	Store                Store
	Cipher               *secrets.Cipher
	Config               Config
	Client               *http.Client
	TokenURL, GraphQLURL string
}
type credential struct {
	Access    string    `json:"access_token"`
	Refresh   string    `json:"refresh_token"`
	ExpiresIn int       `json:"expires_in"`
	Expires   time.Time `json:"expires"`
}

func New(store Store, cipher *secrets.Cipher, cfg Config) (*Service, error) {
	u, err := url.Parse(cfg.RedirectURL)
	if cipher == nil || cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.WebhookSecret == "" || err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && u.Hostname() == "localhost")) {
		return nil, errors.New("Linear requires client ID, client secret, webhook secret, secret cipher and HTTPS callback URL")
	}
	return &Service{Store: store, Cipher: cipher, Config: cfg, Client: &http.Client{Timeout: 15 * time.Second}, TokenURL: "https://api.linear.app/oauth/token", GraphQLURL: "https://api.linear.app/graphql"}, nil
}
func randomSecret() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func (s *Service) Connect(ctx context.Context, p domain.Principal, org string) (string, error) {
	state, verifier := randomSecret(), randomSecret()
	err := s.Store.LinearUpdate(ctx, &p, org, func(st *State) error {
		for k, a := range st.Attempts {
			if time.Now().After(a.Expires) {
				delete(st.Attempts, k)
			}
		}
		if len(st.Attempts) >= 20 {
			return ErrConflict
		}
		st.Attempts[Hash(state)] = &Attempt{Principal: p, Verifier: verifier, Expires: time.Now().Add(10 * time.Minute)}
		return nil
	})
	if err != nil {
		return "", err
	}
	challenge := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {s.Config.ClientID}, "redirect_uri": {s.Config.RedirectURL}, "response_type": {"code"}, "scope": {"read,write,app:assignable,app:mentionable"}, "actor": {"app"}, "prompt": {"consent"}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"}}
	return "https://linear.app/oauth/authorize?" + q.Encode(), nil
}
func (s *Service) token(ctx context.Context, values url.Values) (credential, error) {
	values.Set("client_id", s.Config.ClientID)
	values.Set("client_secret", s.Config.ClientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.TokenURL, strings.NewReader(values.Encode()))
	if err != nil {
		return credential{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.Client.Do(req)
	if err != nil {
		return credential{}, errors.New("Linear authorization is temporarily unavailable")
	}
	defer resp.Body.Close()
	var c credential
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&c) != nil || c.Access == "" || c.Refresh == "" || c.ExpiresIn <= 0 {
		return c, errors.New("Linear authorization failed; reconnect the workspace")
	}
	c.Expires = time.Now().Add(time.Duration(c.ExpiresIn) * time.Second)
	return c, nil
}
func (s *Service) saveToken(org string, c *Connection, token credential) error {
	raw, err := json.Marshal(token)
	if err != nil {
		return err
	}
	c.Encrypted, c.Nonce, err = s.Cipher.Encrypt(raw, "linear:"+org+":"+c.ID)
	return err
}
func (s *Service) access(ctx context.Context, org string, c *Connection) (string, error) {
	if c.Revoked {
		return "", errors.New("Linear installation was removed; reconnect the workspace")
	}
	raw, err := s.Cipher.Decrypt(c.Encrypted, c.Nonce, "linear:"+org+":"+c.ID)
	if err != nil {
		return "", err
	}
	var token credential
	if err = json.Unmarshal(raw, &token); err != nil {
		return "", err
	}
	if time.Until(token.Expires) < time.Minute {
		token, err = s.token(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {token.Refresh}})
		if err != nil {
			return "", err
		}
		if err = s.saveToken(org, c, token); err != nil {
			return "", err
		}
	}
	return token.Access, nil
}
func (s *Service) query(ctx context.Context, token, query string, variables any, out any) error {
	body, _ := json.Marshal(map[string]any{"query": query, "variables": variables})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.GraphQLURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.Client.Do(req)
	if err != nil {
		return errors.New("Linear could not be reached")
	}
	defer resp.Body.Close()
	var result struct {
		Data   json.RawMessage   `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&result) != nil || len(result.Errors) > 0 || len(result.Data) == 0 {
		return fmt.Errorf("Linear request failed (HTTP %d); check workspace permissions or reconnect", resp.StatusCode)
	}
	return json.Unmarshal(result.Data, out)
}
func (s *Service) Callback(ctx context.Context, state, code string) error {
	org, err := s.Store.LinearRoute(ctx, "oauth", Hash(state))
	if err != nil {
		return ErrExpired
	}
	var a *Attempt
	err = s.Store.LinearUpdate(ctx, nil, org, func(st *State) error {
		a = st.Attempts[Hash(state)]
		if a == nil || time.Now().After(a.Expires) {
			return ErrExpired
		}
		delete(st.Attempts, Hash(state))
		return nil
	})
	if err != nil {
		return err
	}
	if code == "" {
		return errors.New("Linear authorization was cancelled")
	}
	token, err := s.token(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {s.Config.RedirectURL}, "code_verifier": {a.Verifier}})
	if err != nil {
		return err
	}
	var data struct {
		Organization Resource                               `json:"organization"`
		Viewer       struct{ ID, Name, DisplayName string } `json:"viewer"`
	}
	if err = s.query(ctx, token.Access, `query { organization { id name } viewer { id name displayName } }`, nil, &data); err != nil {
		return err
	}
	if data.Organization.ID == "" || data.Viewer.ID == "" {
		return ErrInvalid
	}
	return s.Store.LinearUpdate(ctx, &a.Principal, org, func(st *State) error {
		var c *Connection
		for _, v := range st.Connections {
			if v.Workspace.ID == data.Organization.ID {
				c = v
			}
		}
		if c == nil {
			c = &Connection{ID: uuid.NewString()}
		}
		c.Workspace = data.Organization
		c.Agent.ID = data.Viewer.ID
		c.Agent.Name = data.Viewer.Name
		c.Agent.DisplayName = data.Viewer.DisplayName
		c.Error = ""
		c.Revoked = false
		c.RetryAt = time.Time{}
		if err := s.saveToken(org, c, token); err != nil {
			return err
		}
		if err := s.discoverTeams(ctx, token.Access, c); err != nil {
			return err
		}
		st.Connections[c.ID] = c
		return nil
	})
}
func (s *Service) discoverTeams(ctx context.Context, token string, c *Connection) error {
	c.Teams = []Resource{}
	cursor := ""
	for {
		var d struct {
			Teams struct {
				Nodes    []Resource
				PageInfo struct {
					HasNextPage bool
					EndCursor   string
				}
			}
		}
		vars := map[string]any{"after": nil}
		if cursor != "" {
			vars["after"] = cursor
		}
		if err := s.query(ctx, token, `query($after:String) { teams(first:100,after:$after) { nodes { id name } pageInfo { hasNextPage endCursor } } }`, vars, &d); err != nil {
			return err
		}
		c.Teams = append(c.Teams, d.Teams.Nodes...)
		if !d.Teams.PageInfo.HasNextPage {
			return nil
		}
		if d.Teams.PageInfo.EndCursor == "" || d.Teams.PageInfo.EndCursor == cursor {
			return ErrInvalid
		}
		cursor = d.Teams.PageInfo.EndCursor
	}
}
func (s *Service) Projects(ctx context.Context, p domain.Principal, org, connection, team string) ([]Resource, error) {
	result := []Resource{}
	var apiErr error
	err := s.Store.LinearUpdate(ctx, &p, org, func(st *State) error {
		c := st.Connections[connection]
		if c == nil {
			return ErrInvalid
		}
		token, e := s.access(ctx, org, c)
		if e != nil {
			apiErr = e
			c.Error = e.Error()
			return nil
		}
		if e = s.discoverTeams(ctx, token, c); e != nil {
			apiErr = e
			return nil
		}
		allowed := false
		for _, t := range c.Teams {
			if t.ID == team {
				allowed = true
			}
		}
		if !allowed {
			apiErr = ErrInvalid
			return nil
		}
		cursor := ""
		for {
			var d struct {
				Team struct {
					Projects struct {
						Nodes    []Resource
						PageInfo struct {
							HasNextPage bool
							EndCursor   string
						}
					}
				}
			}
			vars := map[string]any{"id": team, "after": nil}
			if cursor != "" {
				vars["after"] = cursor
			}
			if e = s.query(ctx, token, `query($id:String!,$after:String) { team(id:$id) { projects(first:100,after:$after) { nodes { id name } pageInfo { hasNextPage endCursor } } } }`, vars, &d); e != nil {
				apiErr = e
				return nil
			}
			result = append(result, d.Team.Projects.Nodes...)
			if !d.Team.Projects.PageInfo.HasNextPage {
				break
			}
			if d.Team.Projects.PageInfo.EndCursor == cursor || d.Team.Projects.PageInfo.EndCursor == "" {
				return ErrInvalid
			}
			cursor = d.Team.Projects.PageInfo.EndCursor
		}
		c.Error = ""
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, apiErr
}
func (s *Service) SaveProfile(ctx context.Context, p domain.Principal, org string, profile Profile) (*Profile, error) {
	var unchanged *Profile
	if profile.ID != "" {
		err := s.Store.LinearUpdate(ctx, &p, org, func(st *State) error {
			old := st.Profiles[profile.ID]
			if old == nil {
				return ErrInvalid
			}
			if old.ConnectionID == profile.ConnectionID && old.TeamID == profile.TeamID && old.LinearProjectID == profile.LinearProjectID {
				var e error
				unchanged, e = st.SaveProfile(profile, p.UserID)
				return e
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if unchanged != nil {
			return unchanged, nil
		}
	}
	projects, err := s.Projects(ctx, p, org, profile.ConnectionID, profile.TeamID)
	if err != nil {
		return nil, err
	}
	if profile.LinearProjectID != "" {
		found := false
		for _, v := range projects {
			if v.ID == profile.LinearProjectID {
				found = true
			}
		}
		if !found {
			return nil, ErrInvalid
		}
	}
	var result *Profile
	err = s.Store.LinearUpdate(ctx, &p, org, func(st *State) error { var e error; result, e = st.SaveProfile(profile, p.UserID); return e })
	return result, err
}

// Run retries durable activity delivery. A UUID stays fixed across retries.
func (s *Service) Run(ctx context.Context) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		orgs, err := s.Store.LinearOrganizations(ctx)
		if err != nil {
			continue
		}
		for _, org := range orgs {
			if ctx.Err() != nil {
				return
			}
			_ = s.Store.LinearUpdate(ctx, nil, org, func(st *State) error {
				if s.processEvent(ctx, org, st) {
					return nil
				}
				for _, d := range st.Outbox {
					if d.Sent {
						continue
					}
					task := st.Tasks[d.Report.Task]
					if task == nil {
						continue
					}
					c := st.Connections[task.ConnectionID]
					if c == nil || c.Revoked || time.Now().Before(c.RetryAt) {
						continue
					}
					token, e := s.access(ctx, org, c)
					if e != nil {
						c.Error = e.Error()
						c.RetryAt = time.Now().Add(time.Minute)
						return nil
					}
					var out struct{ AgentActivityCreate struct{ Success bool } }
					e = s.query(ctx, token, `mutation($input:AgentActivityCreateInput!) { agentActivityCreate(input:$input) { success } }`, map[string]any{"input": map[string]any{"id": d.ProviderID, "agentSessionId": d.Report.Task, "content": map[string]string{"type": d.Report.Kind, "body": d.Report.Body}}}, &out)
					if e != nil {
						c.RetryAt = time.Now().Add(time.Minute)
					}
					if e == nil && out.AgentActivityCreate.Success {
						d.Sent = true
						c.Error = ""
					}
					return nil
				}
				return nil
			})
		}
	}
}
