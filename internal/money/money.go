// Package money represents Thai baht amounts as integer satang (1 baht = 100
// satang). Floating point is never used: 0.1 + 0.2 != 0.3 in binary floating
// point, and a bill splitting app that loses satang loses trust.
package money

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Satang is an amount in 1/100 of a baht. It may be negative, which is how
// balances express "this person is owed money".
type Satang int64

const satangPerBaht = 100

// MaxSatang is the largest amount this package will parse or divide: one
// billion baht, which is far past any dinner bill and far short of the point
// where the arithmetic stops being exact.
//
// Two things depend on it. SplitByWeight multiplies an amount by a weight, and
// MaxSatang * MaxWeight stays well inside int64. And MaxSatang is comfortably
// below 2^53, so an amount — or a group's running total of many of them — still
// survives a JSON number without losing a satang on the way to the client.
const MaxSatang Satang = 100_000_000_000

// maxBaht is MaxSatang expressed in whole baht, checked before the multiply in
// ParseBaht so that the multiply itself cannot overflow.
const maxBaht = int64(MaxSatang) / satangPerBaht

// MaxWeight bounds a single participant's weight in SplitByWeight. Weights are
// relative, so a caller who needs a bigger ratio than 10,000:1 can scale the
// whole slice down instead.
const MaxWeight int64 = 10_000

var (
	// ErrBadAmount reports a string that is not a well formed baht amount.
	ErrBadAmount = errors.New("money: malformed amount")
	// ErrTooLarge reports a value beyond the range this package divides
	// exactly. Client input reaches these functions directly, so the bound is
	// enforced here rather than trusted to the caller.
	ErrTooLarge = errors.New("money: value out of range")
	// ErrNegative reports a negative amount where only positive is meaningful.
	ErrNegative = errors.New("money: amount must be positive")
	// ErrNoParticipants reports a split with nobody to split between.
	ErrNoParticipants = errors.New("money: need at least one participant")
	// ErrSharesMismatch reports shares that do not sum to the bill total.
	ErrSharesMismatch = errors.New("money: shares do not sum to total")
)

// ParseBaht converts human input such as "1,234.5" or "89" into Satang.
// At most two decimal places are accepted; a third digit is a typo, not a
// rounding opportunity, so it is rejected rather than silently truncated.
// Amounts beyond MaxSatang are ErrBadAmount rather than a wrapped int64.
func ParseBaht(s string) (Satang, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", ""))
	if s == "" {
		return 0, ErrBadAmount
	}

	neg := false
	switch s[0] {
	case '-':
		neg, s = true, s[1:]
	case '+':
		s = s[1:]
	}

	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" && !hasFrac {
		return 0, ErrBadAmount
	}
	if whole == "" {
		whole = "0"
	}

	// strconv.ParseInt accepts its own sign, so "--5" and "1.-5" would parse as
	// numbers here and come out as a positive amount the caller never typed.
	// Both halves must be bare digits.
	if !allDigits(whole) {
		return 0, ErrBadAmount
	}

	baht, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || baht > maxBaht {
		return 0, ErrBadAmount
	}

	var satang int64
	if hasFrac {
		switch len(frac) {
		case 1:
			frac += "0"
		case 2:
		default:
			return 0, ErrBadAmount
		}
		if !allDigits(frac) {
			return 0, ErrBadAmount
		}
		if satang, err = strconv.ParseInt(frac, 10, 64); err != nil {
			return 0, ErrBadAmount
		}
	}

	// baht is bounded above, so this multiply cannot wrap.
	total := Satang(baht*satangPerBaht + satang)
	if total > MaxSatang {
		return 0, ErrBadAmount
	}
	if neg {
		total = -total
	}
	return total, nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// String renders the amount as plain baht with two decimals, e.g. "-12.05".
// It carries no thousands separator or currency symbol so that it round trips
// through ParseBaht; use Format for display.
func (s Satang) String() string {
	sign := ""
	v := int64(s)
	if v < 0 {
		sign, v = "-", -v
	}
	return fmt.Sprintf("%s%d.%02d", sign, v/satangPerBaht, v%satangPerBaht)
}

// Format renders the amount for humans, with thousands separators: "1,234.56".
func (s Satang) Format() string {
	raw := s.String()
	sign := ""
	if strings.HasPrefix(raw, "-") {
		sign, raw = "-", raw[1:]
	}
	whole, frac, _ := strings.Cut(raw, ".")

	var b strings.Builder
	for i, digit := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(digit)
	}
	return sign + b.String() + "." + frac
}

// Abs returns the magnitude of the amount.
func (s Satang) Abs() Satang {
	if s < 0 {
		return -s
	}
	return s
}

// SplitEqual divides total between n people as evenly as integer satang allow.
//
// The remainder cannot vanish, so it is handed out one satang at a time to the
// people at the front of the slice. Callers decide what "front" means: order
// the participants so that the payer absorbs the extra, or rotate the order
// per bill so the same person is not always charged the odd satang.
//
// The returned shares always sum exactly to total.
func SplitEqual(total Satang, n int) ([]Satang, error) {
	if n <= 0 {
		return nil, ErrNoParticipants
	}
	if total < 0 {
		return nil, ErrNegative
	}

	base := total / Satang(n)
	remainder := int(total % Satang(n))

	shares := make([]Satang, n)
	for i := range shares {
		shares[i] = base
		if i < remainder {
			shares[i]++
		}
	}
	return shares, nil
}

// SplitByWeight divides total in proportion to weights — three people sharing a
// pizza where one ate half is weights{2, 1, 1}.
//
// Shares are floored, then the leftover satang go to the participants with the
// largest discarded fraction, which keeps every share within one satang of its
// exact proportional value. Ties break toward the earlier index.
//
// The weights come from the request body, so they are bounded before anything
// is multiplied by them: an unchecked weight overflows total*w, which makes the
// floored shares nonsense and leaves more satang to hand out than there are
// participants to hand them to.
func SplitByWeight(total Satang, weights []int64) ([]Satang, error) {
	if len(weights) == 0 {
		return nil, ErrNoParticipants
	}
	if total < 0 {
		return nil, ErrNegative
	}
	if total > MaxSatang {
		return nil, fmt.Errorf("%w: total %s exceeds %s", ErrTooLarge, total, MaxSatang)
	}

	var sum int64
	for _, w := range weights {
		if w <= 0 {
			return nil, fmt.Errorf("money: weight must be positive, got %d", w)
		}
		if w > MaxWeight {
			return nil, fmt.Errorf("%w: weight %d exceeds %d", ErrTooLarge, w, MaxWeight)
		}
		if sum > math.MaxInt64-w {
			return nil, fmt.Errorf("%w: weights sum past int64", ErrTooLarge)
		}
		sum += w
	}

	shares := make([]Satang, len(weights))
	remainders := make([]int64, len(weights))
	assigned := Satang(0)
	for i, w := range weights {
		exact := int64(total) * w
		shares[i] = Satang(exact / sum)
		remainders[i] = exact % sum
		assigned += shares[i]
	}

	// Hand out the leftover satang to the largest remainders first.
	for left := total - assigned; left > 0; left-- {
		best, bestRem := -1, int64(-1)
		for i, rem := range remainders {
			if rem > bestRem {
				best, bestRem = i, rem
			}
		}
		if best < 0 {
			// Unreachable while the bounds above hold: flooring n shares leaves
			// at most n-1 satang over, so there is always an unused remainder.
			// Returning beats panicking if that ever stops being true.
			return nil, fmt.Errorf("%w: %s left unassigned", ErrSharesMismatch, left)
		}
		shares[best]++
		remainders[best] = -1
	}
	return shares, nil
}

// ValidateShares checks that manually entered shares add up to the bill total.
// Split helpers guarantee this already; this guards the "let me type each
// person's amount myself" path, where a typo would otherwise create or destroy
// money inside a group.
func ValidateShares(total Satang, shares []Satang) error {
	if len(shares) == 0 {
		return ErrNoParticipants
	}
	var sum Satang
	for _, s := range shares {
		if s < 0 {
			return ErrNegative
		}
		sum += s
	}
	if sum != total {
		return fmt.Errorf("%w: got %s, want %s", ErrSharesMismatch, sum, total)
	}
	return nil
}
