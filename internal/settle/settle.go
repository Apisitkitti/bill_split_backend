// Package settle turns a group's net balances into a short list of payments.
//
// A group of six friends who paid for each other all week may owe money in
// twenty directions, but almost all of it cancels out. What matters is the net
// position of each person, and the fewest transfers that flatten every net
// position to zero.
package settle

import (
	"errors"
	"sort"

	"github.com/OatApisit/billsplit-api/internal/money"
)

// ErrUnbalanced reports balances that do not sum to zero, which means the
// caller's ledger is broken: every satang paid is a satang owed by someone.
var ErrUnbalanced = errors.New("settle: balances do not sum to zero")

// Balance is one person's net position in a group. Positive means the group
// owes them; negative means they owe the group.
type Balance struct {
	UserID string
	Net    money.Satang
}

// Transfer is one payment that moves a group closer to settled.
type Transfer struct {
	From   string       `json:"from"`
	To     string       `json:"to"`
	Amount money.Satang `json:"amount"`
}

// Minimize returns payments that settle every balance in the group.
//
// The algorithm repeatedly matches the largest debtor against the largest
// creditor and transfers the smaller of the two magnitudes, which zeroes at
// least one person per transfer and therefore finishes in at most n-1
// payments. Finding the true minimum is NP-hard (it is subset-sum in
// disguise), but this greedy pass is optimal for the common shapes — one
// person covering dinner, or two payers and a table of guests — and never
// worse than n-1 for the rest.
//
// Zero balances are skipped. Input order does not affect the result: ties
// break on user ID so the same group state always produces the same payment
// list, which matters when the result is shown in a chat and compared between
// members.
func Minimize(balances []Balance) ([]Transfer, error) {
	var debtors, creditors []Balance
	var sum money.Satang

	for _, b := range balances {
		sum += b.Net
		switch {
		case b.Net < 0:
			debtors = append(debtors, b)
		case b.Net > 0:
			creditors = append(creditors, b)
		}
	}
	if sum != 0 {
		return nil, ErrUnbalanced
	}
	if len(debtors) == 0 {
		// Empty, not nil. A nil slice marshals to JSON `null`, and a client
		// reading `transfers.length` on it crashes — which is what a settled
		// group is, and what every brand new group is on its first open.
		return []Transfer{}, nil
	}

	// Largest magnitude first, user ID as the tiebreaker for determinism.
	sort.Slice(debtors, func(i, j int) bool {
		if debtors[i].Net != debtors[j].Net {
			return debtors[i].Net < debtors[j].Net
		}
		return debtors[i].UserID < debtors[j].UserID
	})
	sort.Slice(creditors, func(i, j int) bool {
		if creditors[i].Net != creditors[j].Net {
			return creditors[i].Net > creditors[j].Net
		}
		return creditors[i].UserID < creditors[j].UserID
	})

	transfers := make([]Transfer, 0, len(balances)-1)
	for d, c := 0, 0; d < len(debtors) && c < len(creditors); {
		owed := -debtors[d].Net
		due := creditors[c].Net

		amount := owed
		if due < amount {
			amount = due
		}

		transfers = append(transfers, Transfer{
			From:   debtors[d].UserID,
			To:     creditors[c].UserID,
			Amount: amount,
		})

		debtors[d].Net += amount
		creditors[c].Net -= amount
		if debtors[d].Net == 0 {
			d++
		}
		if creditors[c].Net == 0 {
			c++
		}
	}
	return transfers, nil
}

// Net folds a group's bills into one balance per person.
//
// paid maps a user to the total they laid out; owed maps a user to the total
// of their shares. Everyone appearing in either map gets a balance, including
// people whose position happens to be zero, so the caller can show a complete
// roster. The result is sorted by user ID.
func Net(paid, owed map[string]money.Satang) []Balance {
	seen := make(map[string]struct{}, len(paid)+len(owed))
	for id := range paid {
		seen[id] = struct{}{}
	}
	for id := range owed {
		seen[id] = struct{}{}
	}

	balances := make([]Balance, 0, len(seen))
	for id := range seen {
		balances = append(balances, Balance{UserID: id, Net: paid[id] - owed[id]})
	}
	sort.Slice(balances, func(i, j int) bool {
		return balances[i].UserID < balances[j].UserID
	})
	return balances
}
