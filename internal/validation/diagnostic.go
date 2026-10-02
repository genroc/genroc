package validation

// What a definition's failures are, and where. specs/language-server.md §2.

import (
	"errors"
	"sort"
	"strings"

	"genroc/internal/schema"
)

// Code classifies a diagnostic. Deliberately NOT errcode.Code: those are faults an `on_error`
// rule can catch, and sharing the type would put uncatchable codes in a `case:` autocomplete.
type Code string

const (
	// CodeExpression is an expression that did not type-check against its slot's context.
	CodeExpression Code = "def.expression"
	// CodeUnknownRead is reading THROUGH the top type ({}). Its own code because recovery
	// produces it downstream of a failed slot, and only these are suppressed as derived.
	CodeUnknownRead Code = "def.unknown_read"
	// CodeStructure is a rule about the definition's shape rather than its types —
	// reachability, a goto naming no task, a schema document that will not parse.
	CodeStructure Code = "def.structure"
)

// Diagnostic is one reason a definition is not registrable, addressed by the slot grammar of
// specs/schema-command.md §2.
type Diagnostic struct {
	Address string `json:"address"`
	Code    Code   `json:"code"`
	Message string `json:"message"`
	// Location is the field to underline (`tasks.a.action.url`); Address is the scope `genctl
	// schema context` answers about (`tasks.a.action`). Defaults to Address.
	Location string `json:"location"`
}

// Error is the message alone: the prose already names the task and slot.
func (d Diagnostic) Error() string { return d.Message }

// Diagnostics is every failure found in one pass, ordered by address so a run reads the same
// twice. It is an error so a caller that only reports can stay unchanged.
type Diagnostics []Diagnostic

func (ds Diagnostics) Error() string {
	msgs := make([]string, len(ds))
	for i, d := range ds {
		msgs[i] = d.Error()
	}
	return strings.Join(msgs, "; ")
}

// bag keeps at most ONE diagnostic per address: a slot is the unit a reader fixes.
type bag struct {
	found    map[string]Diagnostic
	poisoned map[string]bool // task id → its output failed and now types as {}
	// sees: task id → ids whose outputs are readable there. Without it the only rule left is "any
	// poison suppresses every unknown read", hiding real errors in tasks that cannot see it.
	sees map[string]map[string]bool
}

func newBag() *bag {
	return &bag{found: map[string]Diagnostic{}, poisoned: map[string]bool{}, sees: map[string]map[string]bool{}}
}

// observe records which outputs each task reads, from the sets inference already computed.
func (b *bag) observe(required, optional map[string][]string) {
	for _, m := range []map[string][]string{required, optional} {
		for id, visible := range m {
			set, ok := b.sees[id]
			if !ok {
				set = map[string]bool{}
				b.sees[id] = set
			}
			for _, v := range visible {
				set[v] = true
			}
		}
	}
}

// add records err against address, unless the slot already has a diagnostic. Reading through
// {} is classified apart because recovery manufactures it: see suppressDerived.
func (b *bag) add(address string, code Code, err error) {
	if err == nil {
		return
	}
	if _, taken := b.found[address]; taken {
		return
	}
	if errors.Is(err, schema.ErrUnknownValue) {
		code = CodeUnknownRead
	}
	location := address
	var f *fieldError
	if errors.As(err, &f) && f.field != "" {
		location = address + "." + f.field
	}
	b.found[address] = Diagnostic{Address: address, Code: code, Message: err.Error(), Location: location}
}

// fieldError names the field within a slot a check was looking at. Only checks that already
// know one annotate; the rest underline the whole slot.
type fieldError struct {
	field string
	err   error
}

func (e *fieldError) Error() string { return e.err.Error() }
func (e *fieldError) Unwrap() error { return e.err }

// inField tags err with the field it came from, and is a no-op on nil so it can wrap a call.
func inField(field string, err error) error {
	if err == nil {
		return nil
	}
	return &fieldError{field: field, err: err}
}

// poison marks a task whose own output slot failed. Its exported type became {}, so every
// later read of it fails too — consequences of a diagnostic already reported, not findings.
func (b *bag) poison(taskID string) { b.poisoned[taskID] = true }

func (b *bag) empty() bool { return len(b.found) == 0 }

// diagnostics drops recovery's consequences: an unknown read survives unless its own task can
// see a poisoned output.
func (b *bag) diagnostics() Diagnostics {
	out := make(Diagnostics, 0, len(b.found))
	for _, d := range b.found {
		if d.Code == CodeUnknownRead && b.derived(d.Address) {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out
}

// derived reports whether an unknown read at address is this pass's own doing: the task it
// sits in reads an output that recovery replaced with {}.
func (b *bag) derived(address string) bool {
	seg := strings.Split(address, ".")
	if len(seg) < 2 || seg[0] != slotTasks {
		return false
	}
	// A task sees its OWN output through `self.output`, which `sees` does not list. The output
	// slot itself is exempt: that diagnostic is the CAUSE.
	if b.poisoned[seg[1]] && address != taskSlot(seg[1], slotOutput) {
		return true
	}
	for id := range b.sees[seg[1]] {
		if b.poisoned[id] {
			return true
		}
	}
	return false
}
