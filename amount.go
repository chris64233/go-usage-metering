package usagemetering

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// Amount 表示不可变的十进制精确数量。
// 内部以 带符号整数(val) × 10^-scale 的形式保存，不引入任何浮点误差，
// 适合计量、计费等要求精确运算的场景。
//
// Amount 的零值不保证可用，请使用 ParseAmount 构造；Amount 值可以安全地
// 被多个调用方共享：所有运算都返回新值，绝不修改接收者。
type Amount struct {
	val   *big.Int
	scale uint32
}

// ParseAmount 解析十进制数量字符串，例如 "1"、"-0.25"、"10.100"。
// 仅接受可选符号 + 整数部分 + 可选小数部分的形式，不支持科学计数法，
// 以避免输入环节出现任何精度歧义。末尾多余的零会被规范化去掉。
func ParseAmount(s string) (Amount, error) {
	raw := s
	neg := false
	switch {
	case strings.HasPrefix(s, "-"):
		neg = true
		s = s[1:]
	case strings.HasPrefix(s, "+"):
		s = s[1:]
	}
	if s == "" {
		return Amount{}, fmt.Errorf("invalid amount %q: empty", raw)
	}

	intPart := s
	fracPart := ""
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		intPart = s[:dot]
		fracPart = s[dot+1:]
		if strings.IndexByte(fracPart, '.') >= 0 {
			return Amount{}, fmt.Errorf("invalid amount %q: multiple decimal points", raw)
		}
		if fracPart == "" {
			return Amount{}, fmt.Errorf("invalid amount %q: missing fraction digits", raw)
		}
	}
	if intPart == "" {
		return Amount{}, fmt.Errorf("invalid amount %q: missing integer digits", raw)
	}
	if !isDigits(intPart) || (fracPart != "" && !isDigits(fracPart)) {
		return Amount{}, fmt.Errorf("invalid amount %q: not a decimal number", raw)
	}

	scale := uint32(len(fracPart))
	digits := intPart + fracPart
	val, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return Amount{}, fmt.Errorf("invalid amount %q", raw)
	}
	if neg {
		val.Neg(val)
	}

	a := Amount{val: val, scale: scale}
	a.normalize()
	return a, nil
}

// MustParseAmount 解析数量，失败时 panic。仅用于已知合法的常量或测试。
func MustParseAmount(s string) Amount {
	a, err := ParseAmount(s)
	if err != nil {
		panic(err)
	}
	return a
}

func isDigits(s string) bool {
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

// normalize 去掉末尾多余的零（1.20 -> 1.2, 10.00 -> 10）。
func (a *Amount) normalize() {
	ten := big.NewInt(10)
	zero := big.NewInt(0)
	m := new(big.Int)
	for a.scale > 0 {
		m.Mod(a.val, ten)
		if m.Cmp(zero) != 0 {
			break
		}
		a.val.Quo(a.val, ten)
		a.scale--
	}
}

func (a Amount) intValue() *big.Int {
	if a.val == nil {
		return big.NewInt(0)
	}
	return a.val
}

// aligned 返回把 a、b 对齐到同一小数位后的两个新整数及该小数位。
func aligned(a, b Amount) (*big.Int, *big.Int, uint32) {
	sc := a.scale
	if b.scale > sc {
		sc = b.scale
	}
	va := new(big.Int).Set(a.intValue())
	vb := new(big.Int).Set(b.intValue())
	if a.scale < sc {
		pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(sc-a.scale)), nil)
		va.Mul(va, pow)
	}
	if b.scale < sc {
		pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(sc-b.scale)), nil)
		vb.Mul(vb, pow)
	}
	return va, vb, sc
}

// Add 返回 a+b，精度无损。
func (a Amount) Add(b Amount) Amount {
	va, vb, sc := aligned(a, b)
	r := Amount{val: va.Add(va, vb), scale: sc}
	r.normalize()
	return r
}

// Sub 返回 a-b，精度无损。
func (a Amount) Sub(b Amount) Amount {
	va, vb, sc := aligned(a, b)
	r := Amount{val: va.Sub(va, vb), scale: sc}
	r.normalize()
	return r
}

// Neg 返回 -a。
func (a Amount) Neg() Amount {
	return Amount{val: new(big.Int).Neg(a.intValue()), scale: a.scale}
}

// Cmp 返回 -1/0/1，分别表示 a 小于/等于/大于 b。
func (a Amount) Cmp(b Amount) int {
	va, vb, _ := aligned(a, b)
	return va.Cmp(vb)
}

// Equal 报告两个数量在数值上是否相等（1.0 与 1.00 视为相等）。
func (a Amount) Equal(b Amount) bool {
	return a.Cmp(b) == 0
}

// Sign 返回 -1/0/1。
func (a Amount) Sign() int {
	return a.intValue().Sign()
}

// IsZero 报告数量是否为零。
func (a Amount) IsZero() bool {
	return a.Sign() == 0
}

// String 返回规范化的十进制字符串。
func (a Amount) String() string {
	v := a.intValue()
	if a.scale == 0 {
		return v.String()
	}
	neg := v.Sign() < 0
	abs := new(big.Int).Abs(v)
	digits := abs.String()
	if len(digits) <= int(a.scale) {
		digits = strings.Repeat("0", int(a.scale)+1-len(digits)) + digits
	}
	pos := len(digits) - int(a.scale)
	out := digits[:pos] + "." + digits[pos:]
	if neg {
		out = "-" + out
	}
	return out
}

// MarshalJSON 以字符串形式序列化，保证跨语言、跨解析器精确保真。
func (a Amount) MarshalJSON() ([]byte, error) {
	return json.Marshal(a.String())
}

// UnmarshalJSON 接受 JSON 字符串（首选）或数字字面量。
func (a *Amount) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		// 兼容裸数字字面量。
		s = string(data)
	}
	if s == "" {
		return errors.New("invalid empty amount")
	}
	parsed, err := ParseAmount(s)
	if err != nil {
		return err
	}
	*a = parsed
	return nil
}
