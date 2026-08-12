package repo

import (
	"context"
	"fmt"

	"github.com/OatApisit/billsplit-api/internal/model"
	"github.com/OatApisit/billsplit-api/internal/money"
)

// CreateSettlement records a payment between two members.
func (r *Repo) CreateSettlement(ctx context.Context, s model.Settlement) (*model.Settlement, error) {
	err := r.pool.QueryRow(ctx, `
		INSERT INTO settlements (group_id, from_user, to_user, amount_satang, note)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, created_at`,
		s.GroupID, s.FromUser, s.ToUser, int64(s.Amount), s.Note,
	).Scan(&s.ID, &s.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("repo: insert settlement: %w", err)
	}
	return &s, nil
}

// DeleteSettlement removes a recorded payment, but only for the member who
// recorded it.
//
// A settlement is always filed by its sender — createSettlement takes from_user
// from the authenticated caller and nothing else — so from_user is the row's
// author, and matching on it is the ownership check. Same WHERE-clause
// authorisation and same ErrNotFound as DeleteBill.
func (r *Repo) DeleteSettlement(ctx context.Context, groupID, settlementID, fromUser string) error {
	tag, err := r.pool.Exec(ctx, `
		DELETE FROM settlements
		WHERE id = $1 AND group_id = $2 AND from_user = $3`,
		settlementID, groupID, fromUser)
	if err != nil {
		return notFoundOnMalformedID(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListSettlements returns a group's recorded payments, newest first.
func (r *Repo) ListSettlements(ctx context.Context, groupID string) ([]model.Settlement, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, group_id, from_user, to_user, amount_satang, note, created_at
		FROM settlements WHERE group_id = $1 ORDER BY created_at DESC`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []model.Settlement{}
	for rows.Next() {
		var s model.Settlement
		var amount int64
		if err := rows.Scan(&s.ID, &s.GroupID, &s.FromUser, &s.ToUser,
			&amount, &s.Note, &s.CreatedAt); err != nil {
			return nil, err
		}
		s.Amount = money.Satang(amount)
		out = append(out, s)
	}
	return out, rows.Err()
}
