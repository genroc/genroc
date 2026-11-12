// Package numeric is the one runtime definition of a number — UseNumber decoding, exact literals,
// base-10 arithmetic — shared so evaluation and validation agree. Only / rounds, at a CONSTANT 34
// significant digits so a replay yields the same value. specs/number-precision.md.
package numeric

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strconv"

	"github.com/cockroachdb/apd/v3"
)

// MaxDigits bounds significant digits: a loop multiplying its previous output doubles them each
// tick. Exceeding it is an error, never a rounding -- truncation is what this package prevents.
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

// Format renders d as the canonical json.Number, which round-trips through storage without
// float64. Trailing zeros from division precision are trimmed; the value is unchanged.
func Format(d *apd.Decimal) (json.Number, bool) {
	if d.Form != apd.Finite {
		return "", false
	}
	var reduced apd.Decimal
	reduced.Set(d)
	reduced.Reduce(&reduced)
	return json.Number(reduced.Text('f')), true
}

// Decode keeps numbers as exact literals (json.Number); plain json.Unmarshal corrupts a large
// integer on decode alone. UseNumber only reaches interface{} values, so it is harmless on structs.
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

// DecodeStrict also rejects fields v has no home for. Not a flag on Decode, which also reads
// stored rows, where an unknown field is history; strictness belongs at the entry boundary.
func DecodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if key, ok := firstRecordedUnknown(reflect.ValueOf(v)); ok {
		return fmt.Errorf("json: unknown field %q", key)
	}
	return nil
}

// recordsUnknownKeys is a type whose own decoder cannot see DisallowUnknownFields, and so keeps
// the keys it had no field for instead of refusing them.
type recordsUnknownKeys interface{ UnknownKeys() []string }

// firstRecordedUnknown walks what was decoded, outermost first, as encoding/json reports. It
// does not descend through an interface: no type that records keys is ever held in one.
func firstRecordedUnknown(v reflect.Value) (string, bool) {
	if !v.IsValid() || (v.Kind() == reflect.Pointer && v.IsNil()) {
		return "", false
	}
	if v.CanInterface() {
		if r, ok := v.Interface().(recordsUnknownKeys); ok {
			if keys := r.UnknownKeys(); len(keys) > 0 {
				return keys[0], true
			}
		}
	}
	switch v.Kind() {
	case reflect.Pointer:
		return firstRecordedUnknown(v.Elem())
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				if key, ok := firstRecordedUnknown(v.Field(i)); ok {
					return key, true
				}
			}
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			if key, ok := firstRecordedUnknown(v.Index(i)); ok {
				return key, true
			}
		}
	case reflect.Map:
		keys := v.MapKeys()
		slices.SortFunc(keys, func(a, b reflect.Value) int { return cmp.Compare(fmt.Sprint(a), fmt.Sprint(b)) })
		for _, k := range keys {
			if key, ok := firstRecordedUnknown(v.MapIndex(k)); ok {
				return key, true
			}
		}
	}
	return "", false
}
