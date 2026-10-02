package model

// ObjectRef points at one row in objects. Ref is the content's sha256 truncated to 128 bits,
// hex: object id and change-detection key at once. Size is surfaced without loading the object.
type ObjectRef struct {
	Ref  string `json:"ref"`
	Size int64  `json:"size"`
	// Path is where the value sits inside the slot, empty for the whole slot. Not a marker inside
	// the value: that comes back a plain map, indistinguishable from user data with those keys.
	// specs/object-store.md.
	Path []any `json:"path,omitempty"`
}

// ExternalRef marks this as an unresolved reference for consumers that must not treat one as a
// value. The expression evaluator matches on this method rather than the type: model imports
// expression, so the dependency cannot run the other way. specs/lazy-context.md.
func (r *ObjectRef) ExternalRef() (string, int64) { return r.Ref, r.Size }

// ObjectOwner is the entity that actually carries a reference, so an owner going away touches
// only its own claims; only the sweep sees an object with none left. Reads consult no claim.
// specs/object-store.md.
type ObjectOwner string

const (
	// ObjectOwnerInstance: a live context value-slot, held until the slot stops referencing the
	// hash. OwnerID is the instance.
	ObjectOwnerInstance ObjectOwner = "instance"
	// ObjectOwnerLog: a log payload. OwnerID is the LOG ROW, not the instance, so the claim is
	// wanted exactly while the row is and the prune needs no horizon to say so.
	ObjectOwnerLog ObjectOwner = "log"
)
