package errcode

// Which codes an `on_error` rule can name, and what each one means. The engine.* codes are
// absent by construction: they fail the instance without routing, so no rule ever sees one.

// Kind is which task reports a code, as a mask: a task names the kinds it has — a fetch that is
// only_once is KindFetch|KindOnlyOnce — and an entry matches when the two intersect.
type Kind uint8

const (
	KindFetch    Kind = 1 << iota // a fetch's call
	KindExternal                  // an external task's wait
	KindChild                     // a child task, beside the codes its children raise
	KindOnlyOnce                  // any action, and only where the task is only_once
)

// Info is a catchable code with what reports it and what it means, in the one line a caller
// shows beside it. Every entry is a code the engine stores; the wildcard patterns that cover a
// family are a way of NAMING codes, so they belong to whoever offers them.
type Info struct {
	Code  Code
	Kinds Kind
	Means string
}

// catchable is the whole vocabulary an on_error rule matches against. Every code this package
// declares belongs here or in the terminal engine.* set, and `TestEveryCodeIsClassified` is what
// says so — a code in neither is one an author is never offered and never warned about. The
// status family (HTTP) has no entry: it is unbounded, so only a pattern can stand for it.
var catchable = []Info{
	{HTTPTimeout, KindFetch, "connected, but no response arrived in time"},
	{HTTPDisconnected, KindFetch, "the request went out, the connection broke before a response"},
	{PreTimeout, KindFetch, "timed out before the request was written — it never left"},
	{PreError, KindFetch, "failed before the request was written — it never left"},
	{ResultParse, KindFetch, "the response body was not valid JSON"},
	{ResultTooLarge, KindFetch, "the response body exceeded the size a fetch will read"},
	{ResultInvalid, KindFetch | KindChild, "the result did not satisfy the schema declared for it"},
	{ExternalTimeout, KindExternal, "the wait deadline elapsed"},
	{ExternalLost, KindExternal, "a worker's claim expired without an answer"},
	{OnlyOnceInterrupted, KindOnlyOnce, "a previous attempt was interrupted — route it, never retry"},
}

// Catchable returns the codes a task of these kinds can match, in the order above. The slice is
// the caller's own.
func Catchable(kinds Kind) []Info {
	out := make([]Info, 0, len(catchable))
	for _, info := range catchable {
		if info.Kinds&kinds != 0 {
			out = append(out, info)
		}
	}
	return out
}

// terminal is the other half of the vocabulary: the engine failed the instance itself, so no
// on_error rule ever sees one (errcode.go's engine.* block). Kinds is zero because no task
// reports these — the engine does. `TestEveryTerminalCodeIsListed` keeps it complete, on the
// same standard as catchable: a code with no prose is a bare name wherever it surfaces.
var terminal = []Info{
	{EngineDefinition, 0, "the definition is unusable: missing, or it names a task or goto that is not in it"},
	{EngineExpression, 0, "an expression could not be evaluated against this context"},
	{EngineConfig, 0, "config could not be resolved from the environment"},
	{EngineInput, 0, "an input did not satisfy the schema declared for it"},
	{EngineOutput, 0, "an output did not satisfy the schema declared for it"},
	{EngineSpawn, 0, "spawning children, or arming an external task and reading its answer, failed"},
	{EngineCollect, 0, "collecting a settled batch's outputs failed"},
	{EnginePanic, 0, "a Go panic escaped this instance's advance"},
}

// Terminal returns the codes that fail an instance outright, in declaration order. The slice is
// the caller's own.
func Terminal() []Info { return append([]Info(nil), terminal...) }

// All returns every classified code, catchable first. A caller wanting the whole vocabulary
// must not have to know the mask that covers every Kind.
func All() []Info { return append(Catchable(^Kind(0)), terminal...) }

// Names renders the mask as the task kinds an author would recognise, in declaration order.
// Empty for the zero mask, which is what a terminal code carries.
func (k Kind) Names() []string {
	var out []string
	for _, e := range []struct {
		kind Kind
		name string
	}{
		{KindFetch, "fetch"},
		{KindExternal, "external"},
		{KindChild, "child"},
		{KindOnlyOnce, "only_once"},
	} {
		if k&e.kind != 0 {
			out = append(out, e.name)
		}
	}
	return out
}
