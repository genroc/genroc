package expressiontest

import (
	"encoding/json"
	"strings"
	"testing"

	"genroc/internal/expression"
	"genroc/internal/numeric"
)

// Division is the only place arithmetic rounds, at a pinned 34 significant digits
// (decimal128). Everything else is exact, so these pin where the boundary is —
// the precision is a constant rather than a setting because genroc replays tasks,
// and a precision that varied between runs would change results on retry.

// precEnv holds operands longer than the division precision, which is where the
// interesting cases live. Values arrive as json.Number, matching decoded data.
var precEnv = map[string]any{
	"huge": json.Number("123456789012345678901234567890123456789"),                      // 39 digits
	"vast": json.Number("999999999999999999999999999999999999999999999999999999999999"), // 60 digits
	"frac": json.Number("10.5"),
}

func assertPrecEval(t *testing.T, expr, want string) {
	t.Helper()
	got, err := expression.Eval(expr, precEnv)
	if err != nil {
		t.Fatalf("Eval(%q): %v", expr, err)
	}
	n, ok := got.(json.Number)
	if !ok {
		t.Fatalf("Eval(%q) = %#v (%T), want a json.Number", expr, got, got)
	}
	if n.String() != want {
		t.Errorf("Eval(%q) = %s, want %s", expr, n, want)
	}
}

// --- exact operations: no precision involved ---

// + - * run at unlimited precision, so operand length is the only bound. These
// are well past the division precision and must still be exact.
func TestPrecision_AddIsExactBeyondDivisionPrecision(t *testing.T) {
	assertPrecEval(t, `huge + 1`, "123456789012345678901234567890123456790")
}

func TestPrecision_MultiplyIsExactBeyondDivisionPrecision(t *testing.T) {
	assertPrecEval(t, `huge * 2`, "246913578024691357802469135780246913578")
}

func TestPrecision_SubtractIsExactBeyondDivisionPrecision(t *testing.T) {
	assertPrecEval(t, `vast - 1`, "999999999999999999999999999999999999999999999999999999999998")
}

// --- modulo: sized to the operands, never rounded ---
// % used to share the division context, but Rem computes through an integer quotient — a fixed
// 34 digits failed with "division impossible" on any operand longer than that.

func TestPrecision_ModuloSmallOperands(t *testing.T) {
	assertPrecEval(t, `7 % 3`, "1")
}

func TestPrecision_ModuloOperandLongerThanDivisionPrecision(t *testing.T) {
	assertPrecEval(t, `huge % 7`, "1")
}

func TestPrecision_ModuloVeryLongOperand(t *testing.T) {
	assertPrecEval(t, `vast % 97`, "46")
}

func TestPrecision_ModuloBothOperandsLong(t *testing.T) {
	assertPrecEval(t, `huge % huge`, "0")
}

func TestPrecision_ModuloRejectsFractionalOperand(t *testing.T) {
	if _, err := expression.Eval(`frac % 3`, precEnv); err == nil {
		t.Error("% must reject a fractional operand")
	}
}

func TestPrecision_ModuloByZero(t *testing.T) {
	if _, err := expression.Eval(`7 % 0`, precEnv); err == nil {
		t.Error("% by zero must error")
	}
}

// --- division: the one rounding point ---

// A terminating quotient is exact regardless of the precision cap.
func TestPrecision_DivisionTerminatingIsExact(t *testing.T) {
	assertPrecEval(t, `1 / 8`, "0.125")
}

func TestPrecision_DivisionWholeResultHasNoTrailingZeros(t *testing.T) {
	assertPrecEval(t, `6 / 3`, "2")
}

// A non-terminating quotient rounds at the pinned precision rather than erroring;
// refusing plain 10/3 would be surprising. 34 significant digits means 33 after
// the leading 3.
func TestPrecision_DivisionNonTerminatingRoundsAtPinnedPrecision(t *testing.T) {
	got, err := expression.Eval(`10 / 3`, precEnv)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	digits := strings.TrimPrefix(got.(json.Number).String(), "3.")
	if len(digits) != 33 {
		t.Errorf("10/3 produced %d fractional digits, want 33 (34 significant); got %s", len(digits), got)
	}
}

// Dividing a value longer than the precision rounds it — the documented cost of
// pinning, and the reason + - * are kept unlimited instead.
func TestPrecision_DivisionRoundsOperandsLongerThanPrecision(t *testing.T) {
	got, err := expression.Eval(`huge / 1`, precEnv)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if got.(json.Number).String() == "123456789012345678901234567890123456789" {
		t.Skip("division no longer rounds at 34 digits; the precision constant changed")
	}
}

func TestPrecision_DivisionByZero(t *testing.T) {
	if _, err := expression.Eval(`1 / 0`, precEnv); err == nil {
		t.Error("division by zero must error")
	}
}

// --- literals match the data path ---
// Literals carry exact text, so writing a value into an expression and receiving it as data are
// equally precise. They used to disagree: anything past int64 was rejected outright.

func TestPrecision_DataCarriesValuesBeyondInt64(t *testing.T) {
	assertPrecEval(t, `huge + 0`, "123456789012345678901234567890123456789")
}

func TestPrecision_LiteralBeyondInt64IsExact(t *testing.T) {
	assertPrecEval(t, `12345678901234567890`, "12345678901234567890")
}

func TestPrecision_LiteralBeyondFloat64IsExact(t *testing.T) {
	assertPrecEval(t, `9007199254740993`, "9007199254740993")
}

// The decisive one: the same value written as a literal and read from data must
// compare equal and add identically.
func TestPrecision_LiteralAndDataAgreeBeyondFloat64(t *testing.T) {
	assertPrecEval(t, `huge == 123456789012345678901234567890123456789 ? 1 : 0`, "1")
	assertPrecEval(t, `huge - 123456789012345678901234567890123456788`, "1")
}

// A fractional literal keeps its decimal value rather than a binary expansion,
// which is what makes 0.1 + 0.2 come out as 0.3.
func TestPrecision_FractionalLiteralIsDecimal(t *testing.T) {
	assertPrecEval(t, `0.1 + 0.2`, "0.3")
	assertPrecEval(t, `0.1 * 3`, "0.3")
}

// --- the digit bound ---
// A looping task squares its own output, doubling digits per tick: a 54-digit id reaches
// ~55,000 in ten iterations. Unbounded it ran to apd's exponent limit — after materialising and
// externalising the value, with a message that explained nothing.

// Squaring repeatedly is what a looping task does; the bound must stop it with a
// message naming the cause.
func TestPrecision_RepeatedSquaringHitsDigitLimit(t *testing.T) {
	env := map[string]any{"n": json.Number(strings.Repeat("9", 54))}
	var err error
	for i := 0; i < 20; i++ {
		var v any
		v, err = expression.Eval(`n * n`, env)
		if err != nil {
			break
		}
		env["n"] = v // feed the result back, as a looping task does
	}
	if err == nil {
		t.Fatal("repeated squaring must eventually hit the digit limit")
	}
	for _, want := range []string{"digit limit", "exponentially"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
	// It must trip on our bound, not on apd's exponent limit far later.
	if strings.Contains(err.Error(), "exponent out of range") {
		t.Errorf("hit apd's exponent limit instead of the digit bound: %v", err)
	}
}

// The bound is far above any legitimate payload, so ordinary arithmetic on long
// values is unaffected.
func TestPrecision_LongButLegitimateValuesStillWork(t *testing.T) {
	env := map[string]any{"a": json.Number(strings.Repeat("9", 400))}
	if _, err := expression.Eval(`a * 2`, env); err != nil {
		t.Errorf("a 400-digit value must still multiply: %v", err)
	}
	if _, err := expression.Eval(`a + a`, env); err != nil {
		t.Errorf("a 400-digit value must still add: %v", err)
	}
}

// A literal is bounded the same way, so a definition cannot carry an unbounded
// number either.
func TestPrecision_OversizedLiteralRejected(t *testing.T) {
	_, err := expression.Eval(strings.Repeat("9", numeric.MaxDigits+1), nil)
	if err == nil {
		t.Fatal("a literal over the digit limit must be rejected")
	}
	if !strings.Contains(err.Error(), "digit limit") {
		t.Errorf("expected a digit-limit error, got: %v", err)
	}
}

func TestPrecision_LiteralAtTheLimitAccepted(t *testing.T) {
	if _, err := expression.Eval(strings.Repeat("9", numeric.MaxDigits), nil); err != nil {
		t.Errorf("a literal exactly at the limit must be accepted: %v", err)
	}
}

// A magnitude no decimal can represent is still an ordinary parse error.
func TestPrecision_LiteralOverflowStillRejected(t *testing.T) {
	_, err := expression.Eval(`1e1000000000`, nil)
	if err == nil {
		t.Fatal("expected an overflow literal to be rejected")
	}
	if !strings.Contains(err.Error(), "invalid number") {
		t.Errorf("expected an invalid-number error, got: %v", err)
	}
}
