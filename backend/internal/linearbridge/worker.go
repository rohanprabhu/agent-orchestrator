package linearbridge

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
)

// Worker executes leased commands through the loopback daemon API.
type Worker struct {
	Store                                           *Store
	Coordinator, Token, ProjectID, Harness, RunFile string
	Client                                          *http.Client
	APIPrefix                                       string
}

// Validate pins a worker journal to one coordinator/project. A journal must
// never be reused for another routing scope or copied to a second machine.
func (w *Worker) Validate(ctx context.Context) error {
	if w.APIPrefix != "" && w.APIPrefix != "/api/cloud/v1/linear" {
		return errors.New("unsupported worker API prefix")
	}
	u, err := url.Parse(w.Coordinator)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
		return errors.New("coordinator must be an origin URL")
	}
	if u.Scheme != "https" && (u.Scheme != "http" || u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1") {
		return errors.New("remote coordinator requires HTTPS")
	}
	if len(w.Token) < 32 || w.ProjectID == "" || w.RunFile == "" {
		return errors.New("worker token, Lenticular project ID and run file are required")
	}
	return w.Store.pinScope(ctx, "worker:"+strings.TrimRight(w.Coordinator, "/")+"|"+w.ProjectID)
}

// Run polls until cancellation, retrying transient failures.
func (w *Worker) Run(ctx context.Context) error {
	if err := w.Validate(ctx); err != nil {
		return err
	}
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		if err := w.Step(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("Bridge worker could not complete a poll; will retry", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// Step executes a command and forwards newly observed conversation activity.
func (w *Worker) Step(ctx context.Context) error {
	var c Command
	code, err := w.remote(ctx, "GET", "/worker/commands", nil, &c)
	if err != nil {
		return err
	}
	if code != 204 {
		if err := w.execute(ctx, c); err != nil {
			return err
		}
		// A stop can supersede an in-flight create. Still poll it on the next step.
		code, err = w.remote(ctx, "POST", "/worker/ack", c, nil)
		if err != nil && code != 409 {
			return err
		}
	}
	if err := w.observe(ctx); err != nil {
		return err
	}
	return w.flush(ctx)
}
func (w *Worker) execute(ctx context.Context, c Command) error {
	var session string
	var uncertain int
	err := w.Store.db.QueryRowContext(ctx, `SELECT session,uncertain FROM bindings WHERE task=?`, c.Task).Scan(&session, &uncertain)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if c.Kind == "stop" {
		if uncertain != 0 {
			return w.Store.localReport(ctx, Report{c.ID + ":uncertain", c.Task, "error", "Lenticular session creation has an uncertain outcome. Inspect the local runner and reconcile it before confirming a stop."})
		}
		if session != "" {
			var state struct {
				Session struct {
					Terminated bool `json:"isTerminated"`
				} `json:"session"`
			}
			if err := w.daemon(ctx, "GET", "/sessions/"+url.PathEscape(session), nil, &state); err != nil {
				return err
			}
			if !state.Session.Terminated {
				if err := w.daemon(ctx, "POST", "/sessions/"+url.PathEscape(session)+"/kill", struct{}{}, nil); err != nil {
					return err
				}
			}
		}
		if _, err = w.Store.db.ExecContext(ctx, `UPDATE bindings SET stopped=1 WHERE task=?`, c.Task); err != nil {
			return err
		}
		return w.Store.localReport(ctx, Report{c.ID + ":stopped", c.Task, "response", "Stopped. The Lenticular session is terminated; its work remains available locally for review."})
	}
	if uncertain != 0 {
		return w.Store.localReport(ctx, Report{c.ID + ":uncertain", c.Task, "error", "Lenticular could not confirm session creation. Inspect the local runner and use reconcile before resuming. No duplicate session will be started."})
	}
	if session == "" {
		if c.Kind != "create" {
			return errors.New("local binding is missing; restore the worker journal")
		}
		// An offline daemon has definitely not received a create request.
		if err := w.daemon(ctx, "GET", "/projects/"+url.PathEscape(w.ProjectID), nil, nil); err != nil {
			return err
		}
		// Commit uncertainty BEFORE the non-idempotent daemon call. A crash or lost
		// response can leave an Lenticular session alive, so no automatic second create.
		res, e := w.Store.db.ExecContext(ctx, `INSERT OR IGNORE INTO bindings(task,uncertain) VALUES(?,1)`, c.Task)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return errors.New("session reservation already exists")
		}
		var created struct {
			Session struct {
				ID string `json:"id"`
			} `json:"session"`
		}
		body := map[string]string{"projectId": w.ProjectID, "kind": "worker", "mode": "chat", "displayName": "Linear task"}
		if w.Harness != "" {
			body["harness"] = w.Harness
		}
		if e = w.daemon(ctx, "POST", "/sessions", body, &created); e != nil {
			return w.Store.localReport(ctx, Report{c.ID + ":uncertain", c.Task, "error", "Lenticular session creation has an uncertain outcome. Inspect Lenticular and reconcile the worker journal before sending further instructions. No duplicate will be launched."})
		}
		session = created.Session.ID
		if session == "" {
			return errors.New("daemon returned no session ID; reconciliation required")
		}
		if _, e = w.Store.db.ExecContext(ctx, `UPDATE bindings SET session=?,uncertain=0 WHERE task=?`, session, c.Task); e != nil {
			return e
		}
	}
	if c.Kind != "create" && c.Kind != "message" {
		return errors.New("unsupported worker command")
	}
	// Recheck lease before injecting a prompt: a stop may have arrived during
	// session creation. POST /worker/check never advances or acknowledges it.
	if _, err = w.remote(ctx, "POST", "/worker/check", c, nil); err != nil {
		return err
	}
	// Chat's clientMessageId makes network retries safe, including initial context.
	if err := w.daemon(ctx, "POST", "/sessions/"+url.PathEscape(session)+"/conversation/messages", map[string]string{"text": c.Body, "clientMessageId": "linear:" + c.ID}, nil); err != nil {
		return err
	}
	return w.Store.localReport(ctx, Report{c.ID + ":accepted", c.Task, "thought", "Lenticular accepted the request in local session `" + session + "`. Follow-up messages here go to the same session."})
}

// Reconcile attaches a known Lenticular session after an uncertain creation. It does
// not create another session or replay already-acknowledged instructions.
func (w *Worker) Reconcile(ctx context.Context, task, session string) error {
	if err := w.Validate(ctx); err != nil {
		return err
	}
	var state struct {
		Session struct {
			ID, ProjectID, Mode string
			Terminated          bool `json:"isTerminated"`
		} `json:"session"`
	}
	if err := w.daemon(ctx, "GET", "/sessions/"+url.PathEscape(session), nil, &state); err != nil {
		return err
	}
	if state.Session.ID != session || state.Session.ProjectID != w.ProjectID || state.Session.Mode != "chat" || state.Session.Terminated {
		return errors.New("session must be a live Chat session in the configured project")
	}
	res, err := w.Store.db.ExecContext(ctx, `UPDATE bindings SET session=?,uncertain=0 WHERE task=? AND uncertain=1`, session, task)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return errors.New("no uncertain binding for this task")
	}
	return nil
}
func (w *Worker) daemon(ctx context.Context, method, path string, body, out any) error {
	info, err := runfile.Read(w.RunFile)
	if err != nil {
		return errors.New("cannot read Lenticular run file")
	}
	if info == nil || info.Port < 1 || info.Port > 65535 {
		return errors.New("local daemon is offline")
	}
	_, err = doJSON(ctx, w.Client, method, fmt.Sprintf("http://127.0.0.1:%d/api/v1%s", info.Port, path), "", body, out)
	return err
}
func (w *Worker) remote(ctx context.Context, method, path string, body, out any) (int, error) {
	return doJSON(ctx, w.Client, method, strings.TrimRight(w.Coordinator, "/")+w.APIPrefix+path, w.Token, body, out)
}
func doJSON(ctx context.Context, client *http.Client, method, target, token string, body, out any) (int, error) {
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			return 0, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cache-Control", "no-store")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := client.Do(req)
	if err != nil {
		return 0, errors.New("HTTP transport unavailable")
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return res.StatusCode, fmt.Errorf("HTTP %d", res.StatusCode)
	}
	if out != nil && res.StatusCode != http.StatusNoContent {
		err = json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(out)
	}
	return res.StatusCode, err
}
func (w *Worker) flush(ctx context.Context) error {
	rows, err := w.Store.db.QueryContext(ctx, `SELECT id,task,kind,body FROM reports WHERE sent=0 ORDER BY rowid LIMIT 50`)
	if err != nil {
		return err
	}
	var reports []Report
	for rows.Next() {
		var r Report
		if err = rows.Scan(&r.ID, &r.Task, &r.Kind, &r.Body); err != nil {
			break
		}
		reports = append(reports, r)
	}
	defer func() { _ = rows.Close() }()
	rowErr := rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	if rowErr != nil {
		return rowErr
	}
	for _, r := range reports {
		if _, err = w.remote(ctx, "POST", "/worker/reports", r, nil); err != nil {
			return err
		}
		if _, err = w.Store.db.ExecContext(ctx, `UPDATE reports SET sent=1 WHERE id=?`, r.ID); err != nil {
			return err
		}
	}
	return nil
}
