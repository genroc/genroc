package model

import "fmt"

// Envelope is the on-disk representation of a value-slot (process input, a task
// output, the process output, an external payload). A slot is ALWAYS stored as an
// envelope, never as a raw value, so user data is always nested under Data and is
// never confused with the envelope itself — there is no in-band sentinel to collide
// with arbitrary user JSON.
//
// Exactly one of Data / Refs is populated:
//   - Data: the value is small enough to keep inline.
//   - Refs: the value lives in process_objects; v1 holds a single root reference
//     (no Path), i.e. the whole slot is externalized.
//
// The shape is intentionally forward-compatible: ObjectRef.Path (granular/nested
// externalization) and an encryption discriminator are additive later without
// changing how existing envelopes decode.
type Envelope struct {
	Data any          `json:"data,omitempty"`
	Refs []*ObjectRef `json:"refs,omitempty"`
	// Preview is a short, human-readable excerpt of an externalized value, set only
	// for log payloads so a log listing can show a snippet without loading the object.
	Preview string `json:"preview,omitempty"`
}

// ObjectRef points at one row in process_objects. Ref is the content address — the
// first 16 bytes (128 bits) of the content's sha256, hex-encoded (32 chars); it
// doubles as the object id and the change-detection key (a re-encoded value with the
// same hash needs no new write). Size is the byte length of the content, surfaced to
// the API without loading the object.
type ObjectRef struct {
	Ref  string `json:"ref"`
	Size int64  `json:"size"`
}

func (e Envelope) IsRef() bool { return len(e.Refs) > 0 }

// ObjectOwner is who holds a claim on an object. It governs LIFETIME and nothing else: reads are
// addressed by content hash and consult no claim, because the address IS the content.
// specs/object-store.md.
type ObjectOwner string

const (
	// ObjectOwnerInstance: a live context value-slot, held until the slot stops referencing the
	// hash. OwnerID is the instance.
	ObjectOwnerInstance ObjectOwner = "instance"
	// ObjectOwnerLog: a (pre-redacted) log payload. OwnerID is the instance; the claim carries
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
