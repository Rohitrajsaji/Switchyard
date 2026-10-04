package audit

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Entry struct {
	ID             int64           `json:"id"`
	ActorID        string          `json:"actor_id"`
	Source         string          `json:"source"`
	ProjectID      string          `json:"project_id"`
	EnvironmentID  string          `json:"environment_id,omitempty"`
	Action         string          `json:"action"`
	RequestID      string          `json:"request_id"`
	Reason         string          `json:"reason"`
	BeforeRevision *int64          `json:"before_revision,omitempty"`
	AfterRevision  *int64          `json:"after_revision,omitempty"`
	Details        json.RawMessage `json:"details"`
	CreatedAt      time.Time       `json:"created_at"`
}

// Record takes the caller's transaction so a successful audit cannot outlive a failed write.
func Record(ctx context.Context, tx pgx.Tx, entry Entry) error {
	if entry.ActorID == "" || entry.RequestID == "" || entry.Reason == "" {
		return errors.New("audit actor, request ID and reason required")
	}
	if entry.Details == nil {
		entry.Details = json.RawMessage(`{}`)
	}
	_, err := tx.Exec(ctx, `INSERT INTO audit_entries(actor_id,source,project_id,environment_id,action,request_id,reason,before_revision,after_revision,details)
	VALUES($1,$2,NULLIF($3,''),NULLIF($4,''),$5,$6,$7,$8,$9,$10)`, entry.ActorID, entry.Source, entry.ProjectID, entry.EnvironmentID, entry.Action, entry.RequestID, entry.Reason, entry.BeforeRevision, entry.AfterRevision, entry.Details)
	return err
}

func List(ctx context.Context, pool *pgxpool.Pool, projectID string, beforeID int64) ([]Entry, error) {
	rows, err := pool.Query(ctx, `SELECT id,actor_id,source,COALESCE(project_id,''),COALESCE(environment_id,''),action,request_id,reason,before_revision,after_revision,details,created_at
	FROM audit_entries WHERE project_id=$1 AND ($2::bigint=0 OR id<$2) ORDER BY id DESC LIMIT 100`, projectID, beforeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := make([]Entry, 0)
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.ActorID, &e.Source, &e.ProjectID, &e.EnvironmentID, &e.Action, &e.RequestID, &e.Reason, &e.BeforeRevision, &e.AfterRevision, &e.Details, &e.CreatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}
