package linearbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// Linear uses an installed app's OAuth access token; never a personal API key.
// Provision/refresh that token outside the pilot process.
type Linear struct {
	Token    string
	Client   *http.Client
	Endpoint string
}

// Publish posts one durable report as a Linear agent activity.
func (l *Linear) Publish(ctx context.Context, r Report) error {
	endpoint := l.Endpoint
	if endpoint == "" {
		endpoint = "https://api.linear.app/graphql"
	}
	payload := map[string]any{
		"query":     `mutation($input: AgentActivityCreateInput!) { agentActivityCreate(input:$input) { success } }`,
		"variables": map[string]any{"input": map[string]any{"agentSessionId": r.Task, "content": map[string]string{"type": r.Kind, "body": r.Body}}},
	}
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+l.Token)
	req.Header.Set("Content-Type", "application/json")
	res, err := l.Client.Do(req)
	if err != nil {
		return errors.New("Linear request failed")
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("Linear returned HTTP %d", res.StatusCode)
	}
	var response struct {
		Data struct {
			Activity struct {
				Success bool `json:"success"`
			} `json:"agentActivityCreate"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&response); err != nil {
		return err
	}
	if len(response.Errors) > 0 || !response.Data.Activity.Success {
		return errors.New("Linear rejected activity")
	}
	return nil
}
