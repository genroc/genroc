// Package numeric holds the one definition of what a number is at runtime.
//
// genroc forwards order ids and monetary amounts, and float64 corrupts both:
// 0.1+0.2 != 0.3, and integers lose precision above 2^53 — including on decode
// alone, before any expression runs. So JSON is decoded with UseNumber and every
// numeric value is carried as its exact literal, with arithmetic and comparison
// done in base-10.
//
// Evaluation (internal/expression) and validation (internal/schema) both compare
// numbers, and they must agree; sharing this package is what stops them drifting.
//
// # Precision
//
// There is deliberately no single global precision. Four policies apply, each
// chosen so that nothing is rounded unless the mathematics forces it:
//
//	literals   exact, bounded by MaxDigits. A literal is normalised to exact
//	           decimal text at parse time, so writing a value into an expression is
//	           as precise as receiving it as data.
//	+ - *      exact, no rounding — a result is never approximated. Bounded only
//	           by MaxDigits, which exists because a *looping task* iterates: it
//	           feeds its own output back as self.previous, so `x * x` doubles the
//	           digit count every tick. Within a single expression growth is linear
//	           (there is no exponentiation operator — ** and ^ are rejected), but
//	           across loop iterations it is exponential.
//	/          rounds at 34 significant digits (IEEE 754-2008 decimal128). The
//	           only rounding point in the language: a non-terminating quotient
//	           has to stop somewhere, and erroring on plain 10/3 would be
//	           surprising. See divisionPrecision in internal/expression/ops.go.
//	%          sized to the operands, floored at the division precision. A
//	           remainder is always smaller than the divisor so nothing is
//	           rounded; the precision only has to carry the intermediate
//	           quotient, which a fixed cap could not for long operands.
//
// A single global cap governing all four was considered and rejected: applying
// one to + - * would silently round arithmetic on a value longer than the cap —
// an id past 34 digits would quietly lose its tail — which is the exact class of
// corruption this package exists to remove. Applying one to literals would
// truncate them at parse time, reintroducing the literal/data asymmetry.
//
// The division precision is a constant rather than a setting for a different
// reason: genroc retries tasks and re-runs children, so a precision that varied
// between runs, or between two workers mid-deploy, would make the same
// expression yield different values on replay. If it ever must vary it belongs
// on the versioned definition, not on the server.
//
// MaxDigits bounds how many significant digits a value may carry, and applies to
// both arithmetic results and literals. It is a safety bound, not a precision
// setting: nothing is ever rounded to fit it, a value that exceeds it is an
// error.
package numeric

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"

	"github.com/cockroachdb/apd/v3"
)

// MaxDigits is the largest number of significant digits a value may carry.
//
// It exists because looping tasks iterate. A task whose output multiplies its own
// previous value doubles its digit count every tick, so a 54-digit id reaches
// ~55,000 digits in ten iterations — where apd's own exponent limit finally trips
// with "exponent out of range", after the value has already been materialised and
// pushed to the object store. This bound stops that far earlier and says what
// actually happened.
//
// 1000 digits is far past any legitimate payload: a monetary amount needs ~20, a
// 256-bit hash rendered as decimal 78. Nothing is rounded to fit — exceeding it is
// an error, because silently truncating a number is the failure this package
// exists to prevent.
const MaxDigits = 1000

// ExceedsMaxDigits reports whether d carries more significant digits than a value
// is allowed to have.
func ExceedsMaxDigits(d *apd.Decimal) bool {
	return d.NumDigits() > MaxDigits
}

// ToDecimal converts any runtime numeric representation to an exact decimal.
// Values reach us as json.Number from decoded JSON, as int from expression
// literals, and as int64/float64/float32 from Go-populated contexts and DB scans.
func ToDecimal(v any) (*apd.Decimal, bool) {
	switch n := v.(type) {
	case json.Number:
		d, _, err := apd.NewFromString(n.String())
		return d, err == nil
	case *apd.Decimal:
		return n, true
	case int:
		return apd.New(int64(n), 0), true
	case int64:
		return apd.New(n, 0), true
	case int32:
		return apd.New(int64(n), 0), true
	case float64:
		return fromFloat(n, 64)
	case float32:
		return fromFloat(float64(n), 32)
	}
	return nil, false
}

// fromFloat converts through the shortest text that round-trips, so a value
// written 0.1 becomes decimal 0.1 rather than its binary expansion
// (0.1000000000000000055511151231257827…), which is what the author meant.
func fromFloat(f float64, bits int) (*apd.Decimal, bool) {
	d, _, err := apd.NewFromString(strconv.FormatFloat(f, 'g', -1, bits))
	return d, err == nil
}

// Compare returns -1, 0 or 1 comparing a and b exactly. ok is false unless both
// are numeric.
func Compare(a, b any) (int, bool) {
	x, xok := ToDecimal(a)
	y, yok := ToDecimal(b)
	if !xok || !yok {
		return 0, false
	}
	return x.Cmp(y), true
}

// Equal reports whether a and b are both numeric and numerically equal. It is
// deliberately value-based, not literal-based: 1 and 1.0 are the same number, and
// an enum declared as 1 must keep accepting an input decoded as "1.0".
func Equal(a, b any) bool {
	c, ok := Compare(a, b)
	return ok && c == 0
}

// IsIntegral reports whether v is a whole number.
func IsIntegral(v any) bool {
	d, ok := ToDecimal(v)
	if !ok || d.Form != apd.Finite {
		return false
	}
	var rounded apd.Decimal
	if _, err := apd.BaseContext.RoundToIntegralValue(&rounded, d); err != nil {
		return false
	}
	return rounded.Cmp(d) == 0
}

// Format renders a decimal as the json.Number this language uses as its
// canonical numeric value: it marshals as a bare JSON number and round-trips
// through storage without ever passing through float64. Trailing zeros left by a
// division's precision are trimmed; the value is unchanged.
func Format(d *apd.Decimal) (json.Number, bool) {
	if d.Form != apd.Finite {
		return "", false
	}
	var reduced apd.Decimal
	reduced.Set(d)
	reduced.Reduce(&reduced)
	return json.Number(reduced.Text('f')), true
}

// Decode unmarshals JSON runtime data with numbers preserved as their exact
// literal (json.Number) instead of collapsed into float64.
//
// This is the boundary that matters: plain json.Unmarshal corrupts a large
// integer on decode alone, so a definition that merely forwards an order id
// mangles it before any expression runs. UseNumber only affects values decoded
// into interface{}, so applying it to a typed struct is a no-op — the risk is
// only ever the reverse, forgetting it somewhere data flows in.
func Decode(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}

// DecodeReader is Decode for a stream, e.g. an HTTP request or response body.
func DecodeReader(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	return dec.Decode(v)
}

// DecodeStrict is Decode that also rejects fields v has no home for.
//
// It is deliberately a separate function rather than a flag on Decode: Decode also
// reads rows already written to the database and payloads already accepted from the
// network, where an unrecognised field is history and rejecting it would make stored
// data undecodable. Strictness belongs only at the *entry* boundary, where the sender
// is still there to be told — an API request body, where a misspelled field silently
// becoming a default is a bug the client cannot see.
func DecodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
