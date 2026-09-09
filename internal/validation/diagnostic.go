package validation

// What a definition's failures are, and where. Inference used to spend its location on prose
// ("task %q switch case %q: …") and return the first one, which is why a client could not
// attribute a failure to a field and an editor could not underline it.
// specs/language-server.md §2.

import (
	"errors"
	"sort"
	"strings"

	"genroc/internal/schema"
)

// Code classifies a diagnostic so a consumer can suppress or route a whole class. The
// namespace is `def.` and is deliberately NOT errcode.Code: those are runtime faults an
// `on_error` rule can catch, these make a definition unregistrable and can never be caught.
// Sharing the type would put uncatchable codes in a `case:` author's autocomplete.
type Code string

const (
	// CodeExpression is an expression that did not type-check against its slot's context.
	CodeExpression Code = "def.expression"
	// CodeUnknownRead is reading THROUGH the top type ({}). It is also what recovery
	// produces downstream of a slot that already failed, which is why it is its own code:
	// suppressDerived drops exactly these.
	CodeUnknownRead Code = "def.unknown_read"
	// CodeStructure is a rule about the definition's shape rather than its types —
	// reachability, a goto naming no task, a schema document that will not parse.
	CodeStructure Code = "def.structure"
)

// Diagnostic is one reason a definition is not registrable, addressed by the slot grammar of
// specs/schema-command.md §2 — so `genctl schema context <address>` answers what could have
// been written where this failed.
type Diagnostic struct {
	Address string `json:"address"`
	Code    Code   `json:"code"`
	Message string `json:"message"`
	// Location is the field to point AT, when the check knew one: `tasks.a.action.url` where
	// Address is `tasks.a.action`. The two differ because they answer different questions —
	// Address is the scope a reader can ask `genctl schema context` about, Location is the
	// line to underline. Defaults to Address.
	Location string `json:"location"`
}

// Error is the message alone. The address is the machine-readable location and does not
// belong in prose that already names the task and slot — prefixing would say it twice.
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

// bag collects diagnostics during one inference pass and remembers which slots produced one.
//
// At most ONE diagnostic per address: a slot is the unit a reader fixes, and a second finding
// under the same address is almost always the first one seen again from another angle.
type bag struct {
	found    map[string]Diagnostic
	poisoned map[string]bool // task id → its output failed and now types as {}
	// sees is task id → the ids whose outputs are readable there, so a consequence of
	// recovery can be told from a {} the author wrote. Without it the only available rule
	// would be "any poison suppresses every unknown read", which hides real errors in tasks
	// that cannot even see the poisoned output.
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

// fieldError names the field within a slot that a check was looking at. A slot is small, so
// only the checks that already know a field name annotate; the rest underline the slot, which
// is where the reader has to look anyway.
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

// diagnostics returns what was found, address-ordered, with the consequences of recovery
// removed. An unknown read survives unless the slot's own task can see a poisoned output:
// then the {} is one the author wrote, and refusing to read through it is the answer.
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
	// A task sees its OWN output through `self.output`, and `sees` lists only the other tasks
	// whose outputs are in scope — so without this a task whose output failed reports the
	// failure and then reports its own switch failing to read it. The output slot itself is
	// excluded: that diagnostic is the CAUSE, and dropping it would leave the recovery
	// explaining nothing.
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
