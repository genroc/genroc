package model

import "fmt"

// Envelope is the on-disk representation of a value-slot (process input, a task
// output, the process output, an external payload). A slot is ALWAYS stored as an
// envelope, never as a raw value, so user data is always nested under Data and is
// never confused with the envelope itself — there is no in-band sentinel to collide
// with arbitrary user JSON.
//
// Data and Refs are populated TOGETHER: the cut moves out the fewest, largest leaves and Data
// keeps everything else, so a value is usually part-inline and part-referenced. Each ref carries
// the Path it was taken from, and Refs is empty when nothing had to move.
type Envelope struct {
	Data any          `json:"data,omitempty"`
	Refs []*ObjectRef `json:"refs,omitempty"`
}

// ObjectRef points at one row in objects. Ref is the content address — the
// first 16 bytes (128 bits) of the content's sha256, hex-encoded (32 chars); it
// doubles as the object id and the change-detection key (a re-encoded value with the
// same hash needs no new write). Size is the byte length of the content, surfaced to
// the API without loading the object.
type ObjectRef struct {
	Ref  string `json:"ref"`
	Size int64  `json:"size"`
	// Path is where the value belongs inside the slot, as keys from the slot's root: object
	// keys as strings, array indices as numbers. Empty means the whole slot.
	//
	// It is what lets a composite carry a reference for ONE of its leaves -- a task input whose
	// code is a definition-owned object and whose other fields are per-instance data. Without
	// it the only way to record that is a marker inside the value, which does not survive the
	// round trip: a *ObjectRef marshals to {"ref":…,"size":…} and comes back a plain map, and
	// recovering the type means guessing from the shape, which misreads user data that
	// legitimately has those keys. specs/object-store.md.
	Path []any `json:"path,omitempty"`
}

func (e Envelope) IsRef() bool { return len(e.Refs) > 0 }

// ExternalRef marks this as an unresolved reference for consumers that must not treat one as a
// value. The expression evaluator matches on this method rather than the type: model imports
// expression, so the dependency cannot run the other way. specs/lazy-context.md.
func (r *ObjectRef) ExternalRef() (string, int64) { return r.Ref, r.Size }

// ObjectOwner is who holds a claim on an object. It governs LIFETIME and nothing else: reads are
// addressed by content hash and consult no claim, because the address IS the content.
// specs/object-store.md.
type ObjectOwner string

const (
	// ObjectOwnerInstance: a live context value-slot, held until the slot stops referencing the
	// hash. OwnerID is the instance.
	ObjectOwnerInstance ObjectOwner = "instance"
	// ObjectOwnerLog: a log payload. OwnerID is the instance; the claim carries
	// the retention horizon, so the object outlives the log row that names it.
	ObjectOwnerLog ObjectOwner = "log"
	// ObjectOwnerDefinition: a value embedded in a definition version, OwnerID "name@version".
	// It never expires: nothing deletes a definition version, and an instance pinned to an old
	// one must still be able to load its bundle.
	ObjectOwnerDefinition ObjectOwner = "definition"
	// ObjectOwnerGrace: nobody holds this any more, but a reference was handed out recently.
	// Stamped when a claim is released, so a client that read a reference can still fetch it;
	// OwnerID is empty, one per object. Only owners stamp it -- never the sweep, or an expiring
	// grace claim would earn itself another window forever.
	ObjectOwnerGrace ObjectOwner = "grace"
)

// GraceOwnerID is the owner_id of a grace claim: there is one per object, so it needs no
// subject. Empty rather than a sentinel word, because the kind already says what it is.
const GraceOwnerID = ""

// DefinitionOwnerID is the owner_id a definition version claims objects under.
func DefinitionOwnerID(name string, version int) string {
	return fmt.Sprintf("%s@%d", name, version)
}
