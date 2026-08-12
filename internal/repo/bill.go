package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

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
// The group lock is what makes the check mean anything. Statement order does
// not: a settlement being inserted concurrently locks only its own new row, so
// without the lock the EXISTS below cannot see it, the settlement's own bound is
// computed from a ledger that still contains this bill, and both transactions
// commit — leaving exactly the stranded settlement this function exists to
// refuse. lockGroup and the matching lock in CreateSettlement force the two into
// one order or the other, and in either order the second one is refused.
//
// Within the transaction the deletion is speculative: the DELETE runs first and
// returns the bill's created_at, because the row is gone by the time the check
// needs it, and the rollback is what refuses the delete.
func (r *Repo) DeleteBill(ctx context.Context, groupID, billID, createdBy string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if err := lockGroup(ctx, tx, groupID); err != nil {
		return err
	}

	var billCreatedAt time.Time
	err = tx.QueryRow(ctx, `
		DELETE FROM bills
		WHERE id = $1 AND group_id = $2 AND created_by = $3
		RETURNING created_at`,
		billID, groupID, createdBy).Scan(&billCreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return notFoundOnMalformedID(err)
	}

	depends, err := settlementNotOlderThan(ctx, tx, groupID, billCreatedAt)
	if err != nil {
		return err
	}
	if depends {
		return ErrSettlementDepends
	}

	return tx.Commit(ctx)
}

// settlementNotOlderThan reports whether the group holds any settlement created
// at or after the given instant — the created_at of the bill being deleted.
//
// The rule is deliberately about time rather than about balances. A settlement
// recorded *before* the bill existed provably cannot have been justified by that
// bill, so withdrawing the bill cannot strand it and it is safe to leave behind.
// Anything recorded at or after the bill might have been sent because of it, so
// the bill cannot be pulled out from under it.
//
// The earlier attempt at this asked instead whether any member would be left
// net-positive on settlements alone, and it is not sufficient: the two
// conditions have to hold for the same member, and an attacker can break that
// apart by taking an unrelated, genuine incoming settlement that cancels their
// outgoing one. Their net stays positive while their settlements net to zero, no
// row matches, and the delete proceeds. Ordering does not have that seam —
// whatever else the attacker arranges, the settlement they need to keep is
// younger than the bill they need to drop.
//
// Both timestamps come from the same server and are only ever compared within
// one group, so there is no cross-host clock to reason about. They are
// `DEFAULT clock_timestamp()`, not `now()`, and that difference is load-bearing:
// now() is the transaction's start time, while what this comparison needs to
// mean is whether the settlement could have *seen* the bill — which is commit
// order. CreateBill takes no group lock, so a settlement can BEGIN, block here
// on the group lock while a bill commits, and then be admitted against a ledger
// containing it; stamped at BEGIN it would look older than the bill it was
// justified by, and this query would wave that bill's deletion through.
// Stamping at insert puts the settlement's timestamp after the read that
// admitted it, which is what makes "saw it" imply "younger than it".
//
// The cost is real and accepted: a settlement anywhere in the group freezes
// every bill recorded before it, not only the bill it paid for. The escape hatch
// is unchanged — the sender withdraws the settlement, the author deletes the
// bill, the settlement goes back in.
func settlementNotOlderThan(ctx context.Context, tx pgx.Tx, groupID string, since time.Time) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM settlements
			WHERE group_id = $1 AND created_at >= $2
		)`, groupID, since).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

// HasBills reports whether a group has any expense recorded against it.
func (r *Repo) HasBills(ctx context.Context, groupID string) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM bills WHERE group_id = $1)`, groupID).Scan(&exists)
	return exists, err
}
