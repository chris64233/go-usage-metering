package usagemetering

import (
	"encoding/json"
	"testing"
)

func TestParseAmountAndString(t *testing.T) {
	cases := []struct{ in, out string }{
		{"0", "0"},
		{"1", "1"},
		{"-1", "-1"},
		{"+1.5", "1.5"},
		{"1.20", "1.2"},
		{"10.00", "10"},
		{"-0.25", "-0.25"},
		{"0.100", "0.1"},
		{"123456789012345678901234567890.123456789", "123456789012345678901234567890.123456789"},
	}
	for _, c := range cases {
		a, err := ParseAmount(c.in)
		if err != nil {
			t.Fatalf("ParseAmount(%q) error: %v", c.in, err)
		}
		if got := a.String(); got != c.out {
			t.Errorf("ParseAmount(%q).String() = %q, want %q", c.in, got, c.out)
		}
	}
}

func TestParseAmountInvalid(t *testing.T) {
	for _, in := range []string{"", "-", ".", "1.", "abc", "1.2.3", "1e3", " 1", "1 "} {
		if _, err := ParseAmount(in); err == nil {
			t.Errorf("ParseAmount(%q) expected error", in)
		}
	}
}

func TestAmountArithmeticIsExact(t *testing.T) {
	tenth := MustParseAmount("0.1")
	sum := MustParseAmount("0")
	for i := 0; i < 3; i++ {
		sum = sum.Add(tenth)
	}
	if !sum.Equal(MustParseAmount("0.3")) {
		t.Fatalf("0.1*3 = %s, want 0.3", sum)
	}

	a := MustParseAmount("1.2345678901234567890123456789")
	b := MustParseAmount("9.0000000000000000000000000001")
	if got := a.Add(b).String(); got != "10.234567890123456789012345679" {
		t.Errorf("exact add = %s", got)
	}
	if got := b.Sub(a).String(); got != "7.7654321098765432109876543212" {
		t.Errorf("exact sub = %s", got)
	}
	if got := a.Neg().String(); got != "-1.2345678901234567890123456789" {
		t.Errorf("neg = %s", got)
	}
}

func TestAmountCompare(t *testing.T) {
	if MustParseAmount("1.0").Cmp(MustParseAmount("1.00")) != 0 {
		t.Error("1.0 should equal 1.00")
	}
	if MustParseAmount("-0.1").Cmp(MustParseAmount("0")) >= 0 {
		t.Error("-0.1 should be < 0")
	}
	if MustParseAmount("2").Cmp(MustParseAmount("1.9999999999")) <= 0 {
		t.Error("2 should be > 1.9999999999")
	}
	if !MustParseAmount("0").IsZero() || MustParseAmount("0.0001").IsZero() {
		t.Error("IsZero wrong")
	}
}

func TestAmountJSONRoundTrip(t *testing.T) {
	a := MustParseAmount("-123.4500")
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `"-123.45"` {
		t.Fatalf("marshaled = %s", data)
	}
	var b Amount
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	if !b.Equal(a) {
		t.Errorf("round trip %s != %s", b, a)
	}
	// 兼容裸数字字面量。
	var c Amount
	if err := json.Unmarshal([]byte(`0.3`), &c); err != nil {
		t.Fatal(err)
	}
	if c.String() != "0.3" {
		t.Errorf("literal unmarshal = %s", c)
	}
}

func TestAmountAddDoesNotMutate(t *testing.T) {
	a := MustParseAmount("1.5")
	b := MustParseAmount("2.5")
	_ = a.Add(b)
	if a.String() != "1.5" || b.String() != "2.5" {
		t.Fatal("Add mutated operands")
	}
}
