package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/OatApisit/billsplit-api/internal/model"
	"github.com/OatApisit/billsplit-api/internal/money"
)

// CreateBill stores a bill and its shares atomically.
//
// The caller is responsible for the shares summing to the total; see
// money.ValidateShares. Half-written bills would corrupt every balance in the
// group, so the shares go in with the bill or not at all.
func (r *Repo) CreateBill(ctx context.Context, b model.Bill) (*model.Bill, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	err = tx.QueryRow(ctx, `
		INSERT INTO bills (group_id, payer_id, title, total_satang, note, created_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, created_at`,
		b.GroupID, b.PayerID, b.Title, int64(b.Total), b.Note, b.CreatedBy,
	).Scan(&b.ID, &b.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("repo: insert bill: %w", err)
	}

	for _, s := range b.Shares {
		if _, err := tx.Exec(ctx, `
			INSERT INTO bill_shares (bill_id, user_id, share_satang)
			VALUES ($1, $2, $3)`, b.ID, s.UserID, int64(s.Amount)); err != nil {
			return nil, fmt.Errorf("repo: insert share for %s: %w", s.UserID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &b, nil
}

// ListBills returns a group's bills with their shares, newest first.
//
// Shares are fetched in a second query keyed by group rather than one query
// per bill, which keeps a 50-bill history at two round trips instead of 51.
func (r *Repo) ListBills(ctx context.Context, groupID string) ([]model.Bill, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, group_id, payer_id, title, total_satang, note, created_by, created_at
		FROM bills WHERE group_id = $1 ORDER BY created_at DESC`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	bills := []model.Bill{}
	index := map[string]int{}
	for rows.Next() {
		var b model.Bill
		var total int64
		if err := rows.Scan(&b.ID, &b.GroupID, &b.PayerID, &b.Title, &total,
			&b.Note, &b.CreatedBy, &b.CreatedAt); err != nil {
			return nil, err
		}
		b.Total = money.Satang(total)
		b.Shares = []model.Share{}
		index[b.ID] = len(bills)
		bills = append(bills, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(bills) == 0 {
		return bills, nil
	}

	shareRows, err := r.pool.Query(ctx, `
		SELECT s.bill_id, s.user_id, s.share_satang
		FROM bill_shares s
		JOIN bills b ON b.id = s.bill_id
		WHERE b.group_id = $1`, groupID)
	if err != nil {
		return nil, err
	}
	defer shareRows.Close()

	for shareRows.Next() {
		var billID string
		var s model.Share
		var amount int64
		if err := shareRows.Scan(&billID, &s.UserID, &amount); err != nil {
			return nil, err
		}
		s.Amount = money.Satang(amount)
		if i, ok := index[billID]; ok {
			bills[i].Shares = append(bills[i].Shares, s)
		}
	}
	return bills, shareRows.Err()
}

// DeleteBill removes a bill and, by cascade, its shares — but only for the
// member who created it, and only while no recorded settlement still depends on
// it.
//
// Authorisation is folded into the WHERE clause rather than checked after a
// read: a bill in another group, or one somebody else created, deletes nothing
// and comes back as ErrNotFound, which the handler turns into the same 404 a
// non-member gets. Distinguishing "not yours" from "does not exist" would let a
// member enumerate the bill IDs of groups they cannot see.
//
// The delete, the check, and the commit are one transaction. Doing the check
// first and the delete after would leave a window in which a settlement lands
// between them and is stranded anyway; here the deletion is speculative and the
// rollback is what refuses it.
func (r *Repo) DeleteBill(ctx context.Context, groupID, billID, createdBy string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `
		DELETE FROM bills
		WHERE id = $1 AND group_id = $2 AND created_by = $3`,
		billID, groupID, createdBy)
	if err != nil {
		return notFoundOnMalformedID(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}

	stranded, err := strandedSettlementPayer(ctx, tx, groupID)
	if err != nil {
		return err
	}
	if stranded {
		return ErrSettlementDepends
	}

	return tx.Commit(ctx)
}

// strandedSettlementPayer reports whether the group, as it stands inside this
// transaction, holds a member who is net-positive *and* got there by sending
// settlements.
//
// That combination is the signature of the attack DeleteBill exists to stop.
// A settlement is only ever accepted up to what its sender owes, so sending one
// can move the sender to zero but never past it — unless a bill is retracted
// underneath it afterwards. A member whose settlements have left them owed money
// is therefore claiming a credit that no bill justifies, and the member on the
// other side has no endpoint to undo it: they cannot delete a bill they did not
// author, nor a settlement they did not send.
//
// Note that the bound mirrors createSettlement's: neither party may cross zero.
// Here the same rule is applied to the state the deletion would leave behind.
func strandedSettlementPayer(ctx context.Context, tx pgx.Tx, groupID string) (bool, error) {
	var userID string
	err := tx.QueryRow(ctx, `
		SELECT user_id FROM (
			SELECT payer_id AS user_id, total_satang AS paid, 0 AS owed,
			       0 AS sent, 0 AS received
			FROM bills WHERE group_id = $1

			UNION ALL
			SELECT s.user_id, 0, s.share_satang, 0, 0
			FROM bill_shares s
			JOIN bills b ON b.id = s.bill_id
			WHERE b.group_id = $1

			UNION ALL
			SELECT from_user, amount_satang, 0, amount_satang, 0
			FROM settlements WHERE group_id = $1

			UNION ALL
			SELECT to_user, 0, amount_satang, 0, amount_satang
			FROM settlements WHERE group_id = $1
		) entries
		GROUP BY user_id
		HAVING SUM(paid) - SUM(owed) > 0 AND SUM(sent) - SUM(received) > 0
		LIMIT 1`, groupID).Scan(&userID)

	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// HasBills reports whether a group has any expense recorded against it.
func (r *Repo) HasBills(ctx context.Context, groupID string) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM bills WHERE group_id = $1)`, groupID).Scan(&exists)
	return exists, err
}
