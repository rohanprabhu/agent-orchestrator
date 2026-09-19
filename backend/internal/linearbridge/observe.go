package linearbridge

import (
	"context"
	"net/url"
	"strings"
	"unicode/utf8"
)

type snapshot struct {
	Turns    []struct{ ID, State string } `json:"turns"`
	Messages []struct {
		TurnID, Role, Text string
		Streaming          bool
		Sequence           int64
	} `json:"messages"`
	Activities []struct{ ID, ActivityKind, Status string } `json:"activities"`
}

func (w *Worker) observe(ctx context.Context) error {
	rows, err := w.Store.db.QueryContext(ctx, `SELECT task,session FROM bindings WHERE uncertain=0 AND session!='' AND stopped=0`)
	if err != nil {
		return err
	}
	type binding struct{ task, session string }
	var bindings []binding
	for rows.Next() {
		var b binding
		if err = rows.Scan(&b.task, &b.session); err != nil {
			break
		}
		bindings = append(bindings, b)
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
	for _, b := range bindings {
		var snap snapshot
		if err = w.daemon(ctx, "GET", "/sessions/"+url.PathEscape(b.session)+"/conversation", nil, &snap); err != nil {
			continue
		}
		for _, a := range snap.Activities {
			if a.Status == "pending" && (a.ActivityKind == "approval" || a.ActivityKind == "user_input") {
				if err := w.Store.localReport(ctx, Report{b.task + ":input:" + a.ID, b.task, "elicitation", "Lenticular needs an approval or structured input in local session `" + b.session + "`. Open Lenticular to respond; this bridge does not grant permissions through chat."}); err != nil {
					return err
				}
			}
		}
		for _, turn := range snap.Turns {
			var kind, body string
			switch turn.State {
			case "completed":
				kind = "response"
				for _, m := range snap.Messages {
					if m.TurnID == turn.ID && m.Role == "assistant" && !m.Streaming {
						body += m.Text + "\n\n"
					}
				}
				if strings.TrimSpace(body) == "" {
					body = "Lenticular finished this turn. Review local session `" + b.session + "` for its result."
				}
			case "failed":
				kind = "error"
				body = "The Lenticular turn failed. Inspect local session `" + b.session + "` for details."
			case "interrupted", "cancelled":
				kind = "response"
				body = "The Lenticular turn was interrupted. Its work remains available in local session `" + b.session + "`."
			default:
				continue
			}
			if err := w.Store.localReport(ctx, Report{b.task + ":turn:" + turn.ID, b.task, kind, bounded(body)}); err != nil {
				return err
			}
		}
	}
	return nil
}
func bounded(s string) string {
	if len(s) <= 20000 {
		return s
	}
	s = s[:19000]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "\n\n[Truncated. Open the local Lenticular session for the full result.]"
}
