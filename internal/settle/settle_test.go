package settle

import (
	"encoding/json"
	"errors"
	"math/rand"
	"testing"

	"github.com/OatApisit/billsplit-api/internal/money"
)

// applyTransfers replays a payment list over the starting balances and returns
// the leftovers. A correct settlement leaves everybody at zero.
func applyTransfers(balances []Balance, transfers []Transfer) map[string]money.Satang {
	final := make(map[string]money.Satang, len(balances))
	for _, b := range balances {
		final[b.UserID] = b.Net
	}
	for _, t := range transfers {
		final[t.From] += t.Amount
		final[t.To] -= t.Amount
	}
	return final
}

func assertSettled(t *testing.T, balances []Balance, transfers []Transfer) {
	t.Helper()
	for id, left := range applyTransfers(balances, transfers) {
		if left != 0 {
			t.Errorf("%s left with %s after settlement", id, left)
		}
	}
	for _, tr := range transfers {
		if tr.Amount <= 0 {
			t.Errorf("transfer %s -> %s has non-positive amount %d", tr.From, tr.To, tr.Amount)
		}
		if tr.From == tr.To {
			t.Errorf("transfer from %s to itself", tr.From)
		}
	}
}

// One person covers dinner for the table: everyone pays them directly, and
// there is no reason for any other payment to exist.
func TestMinimizeOnePayer(t *testing.T) {
	balances := []Balance{
		{"payer", 30000},
		{"a", -10000},
		{"b", -10000},
		{"c", -10000},
	}
	transfers, err := Minimize(balances)
	if err != nil {
		t.Fatal(err)
	}
	if len(transfers) != 3 {
		t.Fatalf("got %d transfers, want 3: %+v", len(transfers), transfers)
	}
	for _, tr := range transfers {
		if tr.To != "payer" {
			t.Errorf("transfer went to %s, want payer", tr.To)
		}
	}
	assertSettled(t, balances, transfers)
}

// A chain of debts collapses: a owes b, b owes c, so a should just pay c.
func TestMinimizeCollapsesChain(t *testing.T) {
	balances := []Balance{
		{"a", -10000},
		{"b", 0},
		{"c", 10000},
	}
	transfers, err := Minimize(balances)
	if err != nil {
		t.Fatal(err)
	}
	if len(transfers) != 1 {
		t.Fatalf("got %d transfers, want 1: %+v", len(transfers), transfers)
	}
	if transfers[0].From != "a" || transfers[0].To != "c" {
		t.Errorf("got %s -> %s, want a -> c", transfers[0].From, transfers[0].To)
	}
	assertSettled(t, balances, transfers)
}

func TestMinimizeAlreadySettled(t *testing.T) {
	transfers, err := Minimize([]Balance{{"a", 0}, {"b", 0}})
	if err != nil {
		t.Fatal(err)
	}
	if len(transfers) != 0 {
		t.Errorf("got %d transfers for a settled group, want 0", len(transfers))
	}
}

func TestMinimizeRejectsUnbalanced(t *testing.T) {
	_, err := Minimize([]Balance{{"a", -100}, {"b", 50}})
	if !errors.Is(err, ErrUnbalanced) {
		t.Errorf("got %v, want ErrUnbalanced", err)
	}
}

// Same group state, different input order, same payment list — the result is
// shown in a group chat, so two members must never see different instructions.
func TestMinimizeIsDeterministic(t *testing.T) {
	balances := []Balance{
		{"a", -5000}, {"b", -5000}, {"c", 3000}, {"d", 7000},
	}
	want, err := Minimize(balances)
	if err != nil {
		t.Fatal(err)
	}

	rng := rand.New(rand.NewSource(1))
	for range 20 {
		shuffled := append([]Balance(nil), balances...)
		rng.Shuffle(len(shuffled), func(i, j int) {
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		})

		got, err := Minimize(shuffled)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("shuffle changed transfer count: %d vs %d", len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("shuffle changed transfer %d: %+v vs %+v", i, got[i], want[i])
			}
		}
	}
}

// Whatever the group looks like, the settlement must clear it in at most n-1
// payments without inventing or destroying satang.
func TestMinimizeRandomGroups(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	ids := []string{"a", "b", "c", "d", "e", "f", "g", "h"}

	for range 500 {
		n := 2 + rng.Intn(len(ids)-1)
		balances := make([]Balance, n)

		var running money.Satang
		for i := range n - 1 {
			net := money.Satang(rng.Intn(200001) - 100000)
			balances[i] = Balance{ids[i], net}
			running += net
		}
		// The last person absorbs the rest so the ledger sums to zero.
		balances[n-1] = Balance{ids[n-1], -running}

		transfers, err := Minimize(balances)
		if err != nil {
			t.Fatalf("balances %+v: %v", balances, err)
		}
		if len(transfers) > n-1 {
			t.Errorf("balances %+v produced %d transfers, want at most %d",
				balances, len(transfers), n-1)
		}
		assertSettled(t, balances, transfers)
	}
}

func TestNet(t *testing.T) {
	paid := map[string]money.Satang{"a": 30000, "b": 6000}
	owed := map[string]money.Satang{"a": 12000, "b": 12000, "c": 12000}

	balances := Net(paid, owed)
	if len(balances) != 3 {
		t.Fatalf("got %d balances, want 3", len(balances))
	}

	want := map[string]money.Satang{"a": 18000, "b": -6000, "c": -12000}
	for _, b := range balances {
		if b.Net != want[b.UserID] {
			t.Errorf("%s net = %s, want %s", b.UserID, b.Net, want[b.UserID])
		}
	}

	// Net output feeds straight into Minimize, so it must balance.
	transfers, err := Minimize(balances)
	if err != nil {
		t.Fatal(err)
	}
	assertSettled(t, balances, transfers)
}

func TestNetIncludesZeroBalances(t *testing.T) {
	balances := Net(
		map[string]money.Satang{"a": 10000},
		map[string]money.Satang{"a": 5000, "b": 5000},
	)
	if len(balances) != 2 {
		t.Fatalf("got %d balances, want 2", len(balances))
	}
}

// Every slice this package hands back gets marshalled straight into an API
// response, and a nil slice becomes JSON `null`. A client reading `.length` on
// that crashes — which is exactly the settled-group case, and the state every
// brand new group is in the first time anyone opens it.
func TestMinimizeMarshalsAsArrayNotNull(t *testing.T) {
	cases := map[string][]Balance{
		"settled group": {{"a", 0}, {"b", 0}},
		"empty ledger":  {},
		"single member": {{"a", 0}},
		"one debtor":    {{"a", -100}, {"b", 100}},
	}

	for name, balances := range cases {
		t.Run(name, func(t *testing.T) {
			transfers, err := Minimize(balances)
			if err != nil {
				t.Fatal(err)
			}
			if transfers == nil {
				t.Fatal("returned a nil slice; it would marshal to JSON null")
			}

			encoded, err := json.Marshal(transfers)
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) == "null" {
				t.Errorf("marshalled to %s, want an array", encoded)
			}
		})
	}
}
