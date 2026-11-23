package pixapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// MaxAmount is the largest amount the API Pix decimal format can carry:
// ten integer digits and two decimals, "9999999999.99".
const MaxAmount Amount = 999_999_999_999

// ErrInvalidAmount is wrapped by every error returned by ParseAmount and by
// Amount's JSON decoding.
var ErrInvalidAmount = errors.New("pixapi: invalid amount")

// Amount is a monetary value in centavos (hundredths of a real).
//
// On the wire the API Pix represents money as a JSON string with exactly two
// decimal places and a dot separator, matching the pattern \d{1,10}\.\d{2}
// ("0.01", "123.45"). Amount marshals to and from that representation, so
// floating point never touches a monetary value.
type Amount int64

// ParseAmount parses an API Pix decimal string ("123.45") into an Amount.
//
// The parser is deliberately strict: one to ten integer digits, a dot and
// exactly two decimal digits. Signs, exponents, commas, whitespace and
// missing or extra decimals are rejected, because PSPs reject them too.
// Leading zeros in the integer part are accepted ("007.00"), as the pattern
// in the specification allows them.
func ParseAmount(s string) (Amount, error) {
	n := len(s)
	if n < 4 || n > 13 || s[n-3] != '.' {
		return 0, fmt.Errorf("%w: %q: want digits, a dot and two decimals", ErrInvalidAmount, s)
	}
	var cents int64
	for i := 0; i < n; i++ {
		if i == n-3 {
			continue
		}
		c := s[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: %q: unexpected character %q", ErrInvalidAmount, s, c)
		}
		cents = cents*10 + int64(c-'0')
	}
	// Ten integer digits plus two decimals cannot exceed MaxAmount, and
	// cannot overflow int64 on the way there.
	return Amount(cents), nil
}

// MustParseAmount is like ParseAmount but panics on error. It is meant for
// constants in tests and examples.
func MustParseAmount(s string) Amount {
	a, err := ParseAmount(s)
	if err != nil {
		panic(err)
	}
	return a
}

// Cents returns the amount as an integer number of centavos.
func (a Amount) Cents() int64 { return int64(a) }

// Valid reports whether a can be represented on the wire: non-negative and
// at most MaxAmount.
func (a Amount) Valid() bool { return a >= 0 && a <= MaxAmount }

// String formats the amount in the API Pix decimal form, e.g. "123.45".
// Negative amounts are formatted with a leading minus sign for diagnostics,
// although they are never valid on the wire.
func (a Amount) String() string {
	v := int64(a)
	sign := ""
	if v < 0 {
		sign = "-"
		v = -v
		if v < 0 { // math.MinInt64
			return "-92233720368547758.08"
		}
	}
	units := strconv.FormatInt(v/100, 10)
	c := v % 100
	return sign + units + "." + string([]byte{byte('0' + c/10), byte('0' + c%10)})
}

// MarshalJSON encodes the amount as a JSON string such as "123.45".
func (a Amount) MarshalJSON() ([]byte, error) {
	if !a.Valid() {
		return nil, fmt.Errorf("%w: %s is out of range", ErrInvalidAmount, a.String())
	}
	return []byte(`"` + a.String() + `"`), nil
}

// UnmarshalJSON decodes a JSON string such as "123.45". JSON numbers are
// rejected: the specification mandates strings, and accepting numbers would
// hide float rounding on the sender's side.
func (a *Amount) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil // leave the value unchanged, like encoding/json does
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("%w: %s is not a JSON string", ErrInvalidAmount, b)
	}
	v, err := ParseAmount(s)
	if err != nil {
		return err
	}
	*a = v
	return nil
}
