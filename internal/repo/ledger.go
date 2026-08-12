package repo

import (
	"context"
	"fmt"

	"github.com/OatApisit/billsplit-api/internal/model"
	"github.com/OatApisit/billsplit-api/internal/money"
)

// maxAggregateSatang bounds any total Ledger will report.
//
// money.MaxSatang caps a single parsed amount, but nothing caps a sum of them:
// enough max-value bills push a group's total past 2^53. These totals cross to
// the client as JSON numbers, which are IEEE 754 doubles in every browser and
// exact only below 2^53, so past that point the client renders a figure the
// server never sent. A group up there has been corrupted or attacked, and
// failing loudly beats serving a number that silently rounds.
const maxAggregateSatang = int64(1) << 53

// checkAggregate rejects a ledger row whose totals have left the range a JSON
// number represents exactly.
func checkAggregate(userID string, paid, owed int64) error {
	if paid > maxAggregateSatang || paid < -maxAggregateSatang ||
		owed > maxAggregateSatang || owed < -maxAggregateSatang {
		return fmt.Errorf("%w: %s paid=%d owed=%d", ErrLedgerOverflow, userID, paid, owed)
	}
	return nil
}

// Ledger totals what each member of a group has paid and what they owe.
//
// The four arms of the union are the four ways money moves. Paying a bill and
// sending a settlement both count as paying; owing a share and receiving a
// settlement both count as owing. Members with no activity are unioned in at
// zero so the roster stays complete — a group where one person has done
// nothing should still show them at 0.00 rather than omit them.
func (r *Repo) Ledger(ctx context.Context, groupID string) ([]model.Ledger, error) {
	return ledger(ctx, r.pool, groupID)
}

// ledger is Ledger's body, taking the querier so that CreateSettlement can read
// the totals its bound is checked against inside the transaction that inserts —
// a read on the pool would be a separate snapshot taken outside the group lock,
// which is the whole thing the lock exists to prevent.
func ledger(ctx context.Context, q querier, groupID string) ([]model.Ledger, error) {
	rows, err := q.Query(ctx, `
		SELECT user_id, SUM(paid)::BIGINT, SUM(owed)::BIGINT
		FROM (
			SELECT user_id, 0 AS paid, 0 AS owed
			FROM group_members WHERE group_id = $1

			UNION ALL
			SELECT payer_id, total_satang, 0
			FROM bills WHERE group_id = $1

			UNION ALL
			SELECT s.user_id, 0, s.share_satang
			FROM bill_shares s
			JOIN bills b ON b.id = s.bill_id
			WHERE b.group_id = $1

			UNION ALL
			SELECT from_user, amount_satang, 0
			FROM settlements WHERE group_id = $1

			UNION ALL
			SELECT to_user, 0, amount_satang
			FROM settlements WHERE group_id = $1
		) entries
		GROUP BY user_id`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ledger := []model.Ledger{}
	for rows.Next() {
		var l model.Ledger
		var paid, owed int64
		if err := rows.Scan(&l.UserID, &paid, &owed); err != nil {
			return nil, err
		}
		if err := checkAggregate(l.UserID, paid, owed); err != nil {
			return nil, err
		}
		l.Paid, l.Owed = money.Satang(paid), money.Satang(owed)
		ledger = append(ledger, l)
	}
	return ledger, rows.Err()
}
