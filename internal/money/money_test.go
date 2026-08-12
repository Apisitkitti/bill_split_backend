package money

import (
	"errors"
	"math"
	"testing"
)

func TestParseBaht(t *testing.T) {
	tests := []struct {
		in   string
		want Satang
	}{
		{"0", 0},
		{"89", 8900},
		{"1,234.56", 123456},
		{"12.5", 1250},
		{"  42.05  ", 4205},
		{".75", 75},
		{"-12.05", -1205},
		{"+7", 700},
	}
	for _, tt := range tests {
		got, err := ParseBaht(tt.in)
		if err != nil {
			t.Errorf("ParseBaht(%q) returned error: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseBaht(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestParseBahtRejectsJunk(t *testing.T) {
	for _, in := range []string{"", "abc", "1.234", "1.2.3", "12b", "-", "1e3"} {
		if got, err := ParseBaht(in); err == nil {
			t.Errorf("ParseBaht(%q) = %d, want error", in, got)
		}
	}
}

// strconv.ParseInt accepts a sign of its own, so a sign buried inside the
// number used to survive: "1.-5" parsed as 100 + (-5) = 95 satang, and "--5"
// as a positive 5 baht. Every digit position must be a digit.
func TestParseBahtRejectsInnerSigns(t *testing.T) {
	for _, in := range []string{"1.-5", "1.+5", "--5", "+-5", "-+5", "1.-0", "- 5"} {
		if got, err := ParseBaht(in); err == nil {
			t.Errorf("ParseBaht(%q) = %d, want error", in, got)
		}
	}
}

// A total large enough to wrap int64 used to come back as a plausible positive
// amount with a nil error, and got stored as a bill total.
func TestParseBahtRejectsOverflow(t *testing.T) {
	tooBig := []string{
		"999999999999999999",  // wraps to a positive
		"92233720368547759",   // wraps to a negative
		"9223372036854775807", // int64 max itself, in baht
		"99999999999999999999999999",
		"1000000000.01", // one satang past MaxSatang
	}
	for _, in := range tooBig {
		if got, err := ParseBaht(in); !errors.Is(err, ErrBadAmount) {
			t.Errorf("ParseBaht(%q) = %d, %v; want ErrBadAmount", in, got, err)
		}
	}

	// The bound itself is still accepted, in both directions.
	for _, in := range []string{"1000000000.00", "-1000000000.00"} {
		if _, err := ParseBaht(in); err != nil {
			t.Errorf("ParseBaht(%q) rejected the limit: %v", in, err)
		}
	}
}

func TestParseBahtRoundTrip(t *testing.T) {
	for _, want := range []Satang{0, 1, 99, 100, 123456, -1205} {
		got, err := ParseBaht(want.String())
		if err != nil {
			t.Fatalf("ParseBaht(%q): %v", want.String(), err)
		}
		if got != want {
			t.Errorf("round trip of %d produced %d", want, got)
		}
	}
}

func TestFormat(t *testing.T) {
	tests := []struct {
		in   Satang
		want string
	}{
		{0, "0.00"},
		{5, "0.05"},
		{123456, "1,234.56"},
		{100000000, "1,000,000.00"},
		{-123456, "-1,234.56"},
	}
	for _, tt := range tests {
		if got := tt.in.Format(); got != tt.want {
			t.Errorf("Satang(%d).Format() = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// The satang that a naive float split would lose must land on somebody.
func TestSplitEqualConservesTotal(t *testing.T) {
	totals := []Satang{10000, 10001, 9999, 1, 0, 333333}
	counts := []int{1, 2, 3, 7, 100}

	for _, total := range totals {
		for _, n := range counts {
			shares, err := SplitEqual(total, n)
			if err != nil {
				t.Fatalf("SplitEqual(%d, %d): %v", total, n, err)
			}
			if len(shares) != n {
				t.Fatalf("SplitEqual(%d, %d) returned %d shares", total, n, len(shares))
			}
			var sum Satang
			for _, s := range shares {
				sum += s
			}
			if sum != total {
				t.Errorf("SplitEqual(%d, %d) sums to %d", total, n, sum)
			}
			// No share may differ from another by more than one satang.
			if shares[0]-shares[n-1] > 1 {
				t.Errorf("SplitEqual(%d, %d) spread too wide: %d vs %d",
					total, n, shares[0], shares[n-1])
			}
		}
	}
}

// The classic case: 100 baht between 3 people.
func TestSplitEqualThreeWay(t *testing.T) {
	shares, err := SplitEqual(10000, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []Satang{3334, 3333, 3333}
	for i := range want {
		if shares[i] != want[i] {
			t.Fatalf("got %v, want %v", shares, want)
		}
	}
}

func TestSplitEqualRejectsBadInput(t *testing.T) {
	if _, err := SplitEqual(100, 0); !errors.Is(err, ErrNoParticipants) {
		t.Errorf("SplitEqual with 0 people: got %v, want ErrNoParticipants", err)
	}
	if _, err := SplitEqual(-100, 2); !errors.Is(err, ErrNegative) {
		t.Errorf("SplitEqual with negative total: got %v, want ErrNegative", err)
	}
}

func TestSplitByWeight(t *testing.T) {
	tests := []struct {
		name    string
		total   Satang
		weights []int64
		want    []Satang
	}{
		{"double share", 10000, []int64{2, 1, 1}, []Satang{5000, 2500, 2500}},
		{"equal via weights", 10000, []int64{1, 1, 1}, []Satang{3334, 3333, 3333}},
		{"single", 999, []int64{5}, []Satang{999}},
		{"largest remainder wins", 1000, []int64{1, 1, 1, 1, 1, 1}, []Satang{167, 167, 167, 167, 166, 166}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SplitByWeight(tt.total, tt.weights)
			if err != nil {
				t.Fatal(err)
			}
			var sum Satang
			for i, s := range got {
				if s != tt.want[i] {
					t.Errorf("share %d = %d, want %d (all: %v)", i, s, tt.want[i], got)
				}
				sum += s
			}
			if sum != tt.total {
				t.Errorf("shares sum to %d, want %d", sum, tt.total)
			}
		})
	}
}

func TestSplitByWeightRejectsBadInput(t *testing.T) {
	if _, err := SplitByWeight(100, nil); !errors.Is(err, ErrNoParticipants) {
		t.Errorf("no weights: got %v, want ErrNoParticipants", err)
	}
	if _, err := SplitByWeight(100, []int64{1, 0}); err == nil {
		t.Error("zero weight: want error")
	}
	if _, err := SplitByWeight(-1, []int64{1}); !errors.Is(err, ErrNegative) {
		t.Errorf("negative total: got %v, want ErrNegative", err)
	}
}

// Weights arrive from the request body. An unbounded one overflows total*w,
// which made the floored shares nonsense, left more satang over than there were
// participants, and panicked with index out of range [-1].
func TestSplitByWeightRejectsOverflowingWeights(t *testing.T) {
	cases := []struct {
		name    string
		total   Satang
		weights []int64
	}{
		{"int64 max", 10000, []int64{math.MaxInt64, 1}},
		{"just past the cap", 10000, []int64{MaxWeight + 1, 1}},
		{"large but not maximal", 10000, []int64{1 << 40, 1 << 40}},
		{"total past the cap", MaxSatang + 1, []int64{1, 1}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SplitByWeight(tt.total, tt.weights)
			if !errors.Is(err, ErrTooLarge) {
				t.Fatalf("SplitByWeight(%d, %v) = %v, %v; want ErrTooLarge",
					tt.total, tt.weights, got, err)
			}
		})
	}

	// The caps themselves still divide, and still conserve every satang.
	shares, err := SplitByWeight(MaxSatang, []int64{MaxWeight, MaxWeight, 1})
	if err != nil {
		t.Fatalf("split at the limits: %v", err)
	}
	var sum Satang
	for _, s := range shares {
		sum += s
	}
	if sum != MaxSatang {
		t.Errorf("shares sum to %d, want %d", sum, MaxSatang)
	}
}

// Whatever the weights, the shares must add back up to the total — that is the
// property the handler relies on when it stores them against a bill.
func TestSplitByWeightConservesTotal(t *testing.T) {
	totals := []Satang{0, 1, 7, 9999, 10000, 333333, MaxSatang}
	weightSets := [][]int64{
		{1},
		{1, 1, 1},
		{2, 1, 1},
		{7, 3},
		{MaxWeight, 1},
		{1, 1, 1, 1, 1, 1, 1},
		{9, 8, 7, 6, 5, 4, 3, 2, 1},
	}
	for _, total := range totals {
		for _, weights := range weightSets {
			shares, err := SplitByWeight(total, weights)
			if err != nil {
				t.Fatalf("SplitByWeight(%d, %v): %v", total, weights, err)
			}
			if len(shares) != len(weights) {
				t.Fatalf("SplitByWeight(%d, %v) returned %d shares",
					total, weights, len(shares))
			}
			var sum Satang
			for _, s := range shares {
				if s < 0 {
					t.Errorf("SplitByWeight(%d, %v) gave a negative share: %v",
						total, weights, shares)
				}
				sum += s
			}
			if sum != total {
				t.Errorf("SplitByWeight(%d, %v) sums to %d", total, weights, sum)
			}
		}
	}
}

func TestValidateShares(t *testing.T) {
	if err := ValidateShares(10000, []Satang{3334, 3333, 3333}); err != nil {
		t.Errorf("exact shares rejected: %v", err)
	}
	if err := ValidateShares(10000, []Satang{3333, 3333, 3333}); !errors.Is(err, ErrSharesMismatch) {
		t.Errorf("short by one satang: got %v, want ErrSharesMismatch", err)
	}
	if err := ValidateShares(100, []Satang{200, -100}); !errors.Is(err, ErrNegative) {
		t.Errorf("negative share: got %v, want ErrNegative", err)
	}
}
