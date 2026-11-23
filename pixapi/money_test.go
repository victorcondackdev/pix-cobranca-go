package pixapi_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/victorcondackdev/pix-cobranca-go/pixapi"
)

func TestParseAmount(t *testing.T) {
	tests := []struct {
		in   string
		want pixapi.Amount
		ok   bool
	}{
		{"0.00", 0, true},
		{"0.01", 1, true},
		{"1.00", 100, true},
		{"123.45", 12345, true},
		{"007.10", 710, true},
		{"9999999999.99", pixapi.MaxAmount, true},
		{"", 0, false},
		{"1", 0, false},
		{"1.0", 0, false},
		{"1.000", 0, false},
		{".50", 0, false},
		{"1,00", 0, false},
		{"-1.00", 0, false},
		{"+1.00", 0, false},
		{" 1.00", 0, false},
		{"1.00 ", 0, false},
		{"1e2.00", 0, false},
		{"1.0a", 0, false},
		{"10000000000.00", 0, false},
		{"1.2.3", 0, false},
	}
	for _, tt := range tests {
		got, err := pixapi.ParseAmount(tt.in)
		if tt.ok != (err == nil) {
			t.Errorf("ParseAmount(%q) error = %v, want ok=%v", tt.in, err, tt.ok)
			continue
		}
		if err != nil && !errors.Is(err, pixapi.ErrInvalidAmount) {
			t.Errorf("ParseAmount(%q) error %v does not wrap ErrInvalidAmount", tt.in, err)
		}
		if got != tt.want {
			t.Errorf("ParseAmount(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestAmountString(t *testing.T) {
	tests := map[pixapi.Amount]string{
		0: "0.00", 1: "0.01", 10: "0.10", 100: "1.00", 12345: "123.45",
		pixapi.MaxAmount: "9999999999.99", -5: "-0.05", -12345: "-123.45",
		-9223372036854775808: "-92233720368547758.08",
	}
	for a, want := range tests {
		if got := a.String(); got != want {
			t.Errorf("Amount(%d).String() = %q, want %q", int64(a), got, want)
		}
	}
	if pixapi.Amount(5).Cents() != 5 {
		t.Error("Cents")
	}
}

func TestAmountJSON(t *testing.T) {
	type wrap struct {
		V pixapi.Amount `json:"v"`
	}
	b, err := json.Marshal(wrap{V: 12345})
	if err != nil || string(b) != `{"v":"123.45"}` {
		t.Fatalf("Marshal = %s, %v", b, err)
	}
	var w wrap
	if err := json.Unmarshal([]byte(`{"v":"0.99"}`), &w); err != nil || w.V != 99 {
		t.Fatalf("Unmarshal = %d, %v", w.V, err)
	}
	for _, bad := range []string{`{"v":1.5}`, `{"v":"1.5"}`, `{"v":null}`, `{"v":"abc"}`} {
		w = wrap{}
		if err := json.Unmarshal([]byte(bad), &w); err == nil && bad != `{"v":null}` {
			t.Errorf("Unmarshal(%s) succeeded", bad)
		}
	}
	if _, err := json.Marshal(wrap{V: -1}); err == nil {
		t.Error("Marshal of a negative amount succeeded")
	}
	if _, err := json.Marshal(wrap{V: pixapi.MaxAmount + 1}); err == nil {
		t.Error("Marshal above MaxAmount succeeded")
	}
}

func TestMustParseAmount(t *testing.T) {
	if pixapi.MustParseAmount("1.50") != 150 {
		t.Fatal("MustParseAmount")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("MustParseAmount did not panic")
		}
	}()
	pixapi.MustParseAmount("1,50")
}

// reference is an independent, obviously correct implementation used to
// cross-check the parser.
func reference(s string) (int64, bool) {
	ip, fp, ok := strings.Cut(s, ".")
	if !ok || len(ip) < 1 || len(ip) > 10 || len(fp) != 2 {
		return 0, false
	}
	for _, c := range ip + fp {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(ip+fp, 10, 64)
	return n, err == nil
}

func FuzzParseAmount(f *testing.F) {
	for _, s := range []string{"0.00", "123.45", "9999999999.99", "1,00", "-1.00", "1.0", "", "00000000000.00", "1.2.3"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		a, err := pixapi.ParseAmount(s)
		want, ok := reference(s)
		if ok != (err == nil) {
			t.Fatalf("ParseAmount(%q) err=%v, reference ok=%v", s, err, ok)
		}
		if err != nil {
			return
		}
		if int64(a) != want {
			t.Fatalf("ParseAmount(%q) = %d, reference %d", s, a, want)
		}
		if !a.Valid() {
			t.Fatalf("ParseAmount(%q) = %d is not Valid", s, a)
		}
		// Formatting is canonical and round-trips.
		back, err := pixapi.ParseAmount(a.String())
		if err != nil || back != a {
			t.Fatalf("round trip of %q: %q -> %d, %v", s, a.String(), back, err)
		}
		b, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		var c pixapi.Amount
		if err := json.Unmarshal(b, &c); err != nil || c != a {
			t.Fatalf("JSON round trip of %q: %s -> %d, %v", s, b, c, err)
		}
	})
}

func ExampleParseAmount() {
	a, err := pixapi.ParseAmount("1234.50")
	if err != nil {
		panic(err)
	}
	fmt.Println(a.Cents(), a)

	_, err = pixapi.ParseAmount("1234,50")
	fmt.Println(err)
	// Output:
	// 123450 1234.50
	// pixapi: invalid amount: "1234,50": want digits, a dot and two decimals
}

func ExampleAmount_MarshalJSON() {
	b, _ := json.Marshal(pixapi.Valor{Original: 1990})
	fmt.Println(string(b))
	// Output: {"original":"19.90"}
}
