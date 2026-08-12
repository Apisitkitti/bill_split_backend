package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/OatApisit/billsplit-api/internal/model"
)

// CreateGroup creates a group and enrols its creator as the first member.
//
// Both statements run in one transaction: a group with no members is
// unreachable — nobody would pass the membership check to open it — so it must
// never exist, not even briefly.
func (r *Repo) CreateGroup(ctx context.Context, name, createdBy, lineGroupID string) (*model.Group, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var lineID *string
	if lineGroupID != "" {
		lineID = &lineGroupID
	}

	var g model.Group
	err = tx.QueryRow(ctx, `
		INSERT INTO groups (name, created_by, line_group_id)
		VALUES ($1, $2, $3)
		RETURNING id, name, created_by, created_at, COALESCE(line_group_id, '')`,
		name, createdBy, lineID,
	).Scan(&g.ID, &g.Name, &g.CreatedBy, &g.CreatedAt, &g.LineGroupID)
	if err != nil {
		return nil, fmt.Errorf("repo: insert group: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO group_members (group_id, user_id) VALUES ($1, $2)`,
		g.ID, createdBy); err != nil {
		return nil, fmt.Errorf("repo: insert creator membership: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &g, nil
}

// GroupByLineID finds the group bound to a LINE chat, so that reopening the
// LIFF app from the same chat lands in the same group instead of making a new
// one every time.
func (r *Repo) GroupByLineID(ctx context.Context, lineGroupID string) (*model.Group, error) {
	var g model.Group
	err := r.pool.QueryRow(ctx, `
		SELECT id, name, created_by, created_at, COALESCE(line_group_id, '')
		FROM groups WHERE line_group_id = $1`, lineGroupID,
	).Scan(&g.ID, &g.Name, &g.CreatedBy, &g.CreatedAt, &g.LineGroupID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// ListGroups returns the groups a user belongs to, newest first.
func (r *Repo) ListGroups(ctx context.Context, userID string) ([]model.Group, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT g.id, g.name, g.created_by, g.created_at, COALESCE(g.line_group_id, '')
		FROM groups g
		JOIN group_members m ON m.group_id = g.id
		WHERE m.user_id = $1
		ORDER BY g.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	groups := []model.Group{}
	for rows.Next() {
		var g model.Group
		if err := rows.Scan(&g.ID, &g.Name, &g.CreatedBy, &g.CreatedAt, &g.LineGroupID); err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

// GetGroup loads a group with its members, but only for a member of it.
//
// Authorisation is folded into the query rather than checked afterwards: a
// separate "is this person a member" call is a step that a future handler can
// forget to make, whereas a WHERE clause cannot be skipped by accident.
func (r *Repo) GetGroup(ctx context.Context, groupID, viewerID string) (*model.Group, error) {
	var g model.Group
	err := r.pool.QueryRow(ctx, `
		SELECT g.id, g.name, g.created_by, g.created_at, COALESCE(g.line_group_id, '')
		FROM groups g
		JOIN group_members m ON m.group_id = g.id AND m.user_id = $2
		WHERE g.id = $1`, groupID, viewerID,
	).Scan(&g.ID, &g.Name, &g.CreatedBy, &g.CreatedAt, &g.LineGroupID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	if g.Members, err = r.Members(ctx, groupID); err != nil {
		return nil, err
	}
	return &g, nil
}

// Members lists everyone in a group, in join order.
func (r *Repo) Members(ctx context.Context, groupID string) ([]model.User, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT u.line_user_id, u.display_name, u.picture_url
		FROM group_members m
		JOIN users u ON u.line_user_id = m.user_id
		WHERE m.group_id = $1
		ORDER BY m.joined_at`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	members := []model.User{}
	for rows.Next() {
		var u model.User
		if err := rows.Scan(&u.ID, &u.DisplayName, &u.PictureURL); err != nil {
			return nil, err
		}
		members = append(members, u)
	}
	return members, rows.Err()
}

// AddMember puts a user in a group. Joining twice is not an error: the second
// tap of a shared invite link should be a no-op, not a failure.
//
// A group ID that does not exist comes back as ErrNotFound rather than a raw
// foreign key violation. The handler turns that into the same 404 a non-member
// gets, so a 500 on the missing row cannot be used to tell real group IDs from
// invented ones.
func (r *Repo) AddMember(ctx context.Context, groupID, userID string) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO group_members (group_id, user_id) VALUES ($1, $2)
		ON CONFLICT DO NOTHING`, groupID, userID)

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
		return ErrNotFound
	}
	return err
}

// IsMember reports whether a user belongs to a group.
//
// A group ID that is not a UUID comes back as ErrNotFound rather than a raw
// Postgres syntax error. This is the first query every group-scoped request
// runs, so without the same treatment DeleteBill gets, a malformed ID would be
// a 500 here before any handler could turn it into the 404 a prober must see.
func (r *Repo) IsMember(ctx context.Context, groupID, userID string) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM group_members WHERE group_id = $1 AND user_id = $2)`,
		groupID, userID).Scan(&exists)
	if err != nil {
		return false, notFoundOnMalformedID(err)
	}
	return exists, nil
}
