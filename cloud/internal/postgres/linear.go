package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/cloud/internal/domain"
	"github.com/aoagents/agent-orchestrator/cloud/internal/linear"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func (s *Store) LinearUpdate(ctx context.Context, p *domain.Principal, org string, fn func(*linear.State) error) error {
	work := func(tx pgx.Tx) error {
		if p != nil {
			if err := requireOrgAdmin(ctx, tx, org, p.UserID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ao_linear_state(org_id) VALUES($1) ON CONFLICT DO NOTHING`, org); err != nil {
			return err
		}
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT state FROM ao_linear_state WHERE org_id=$1 FOR UPDATE`, org).Scan(&raw); err != nil {
			return err
		}
		var state linear.State
		if err := json.Unmarshal(raw, &state); err != nil {
			return err
		}
		state.Init()
		// A removed runner owner must not retain execution authority. Preserve the
		// worker route only so it can drain the durable stop commands.
		for _, profile := range state.Profiles {
			if profile.Disconnected || profile.Suspended {
				continue
			}
			var active bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ao_org_memberships m JOIN ao_organizations o ON o.id=m.org_id WHERE m.org_id=$1 AND m.user_id=$2 AND m.status='active' AND o.status='active' AND m.role IN ('owner','admin'))`, org, profile.Owner).Scan(&active); err != nil {
				return err
			}
			if !active {
				profile.Suspended = true
				profile.Paused = true
				for task, t := range state.Tasks {
					if t.ProfileID == profile.ID {
						state.Enqueue(task, t.ConnectionID, "", "", task+":owner-revoked", "stop", "")
					}
				}
			}
		}
		if err := fn(&state); err != nil {
			return err
		}
		raw, err := json.Marshal(state)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE ao_linear_state SET state=$2 WHERE org_id=$1`, org, raw); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM ao_linear_routes WHERE org_id=$1`, org); err != nil {
			return err
		}
		insert := func(kind, key string) error {
			_, err := tx.Exec(ctx, `INSERT INTO ao_linear_routes(kind,key,org_id) VALUES($1,$2,$3)`, kind, key, org)
			return err
		}
		for key, a := range state.Attempts {
			if time.Now().Before(a.Expires) {
				if err := insert("oauth", key); err != nil {
					return err
				}
			}
		}
		for _, c := range state.Connections {
			if err := insert("workspace", c.Workspace.ID); err != nil {
				return err
			}
		}
		for _, p := range state.Profiles {
			if !p.Disconnected {
				if err := insert("worker", p.Challenge); err != nil {
					return err
				}
			}
		}
		return nil
	}
	var err error
	if p == nil {
		err = s.withOrg(ctx, org, work)
	} else {
		err = s.withTenant(ctx, *p, org, work)
	}
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "23505" {
		return linear.ErrConflict
	}
	return err
}
func (s *Store) linearLookup(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT set_config('ao.service','linear-dispatch',true)`); err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) LinearRoute(ctx context.Context, kind, key string) (org string, err error) {
	err = s.linearLookup(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT org_id::text FROM ao_linear_routes WHERE kind=$1 AND key=$2`, kind, key).Scan(&org)
	})
	return
}
func (s *Store) LinearOrganizations(ctx context.Context) (orgs []string, err error) {
	err = s.linearLookup(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT DISTINCT org_id::text FROM ao_linear_routes WHERE kind='workspace'`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			orgs = append(orgs, id)
		}
		return rows.Err()
	})
	return
}
