package validation

import (
	"fmt"
	"strings"

	"genroc/internal/model"
)

// predEdge is a predecessor edge in the task graph.
// isErr is true for on_error routes: the failing task has no output on this path.
type predEdge struct {
	idx   int  // predecessor task index; -1 = process start
	isErr bool // true = on_error route
	// rule indexes the predecessor's OnError slice for an error edge, -1 otherwise. It is
	// what attributes `error` at a handler to the rules that could have set it, which is the
	// only way to know which statuses — and so which declared bodies — can arrive there.
	rule int
}

// taskHasOutput reports whether a task exports an output to outputs.<id>. Only an
// `output` projection exports; a raw action result (even with a result_schema, or
// a child's output) is transient — available to the task's own output/switch as
// self.result, but never added to the shared context.
func taskHasOutput(s *model.Task) bool {
	return s.Output.Present()
}

// terminalEnd is one way the process can finish: the task it ends on, the task outputs
// guaranteed present there (must) and possibly present (may), and whether `error` is.
type terminalEnd struct {
	task   string
	must   map[string]bool
	may    map[string]bool
	errMin bool
	errMax bool
}

// outputTerminals: one entry per way of ending. Kept apart because outputContextSets
// INTERSECTS must-sets, destroying the correlation that lets `outputs.a.v ?? outputs.b.v`
// type non-null when a and b cover every terminal. specs/path-sensitive-output.md.
func outputTerminals(def *model.ProcessDefinition) []terminalEnd {
	tasks := def.Tasks
	n := len(tasks)
	if n == 0 {
		return nil
	}

	reqMap, optMap, mustErrMap, mayErrMap, _ := computeContextSets(tasks)

	var terminals []terminalEnd

	addTerminal := func(s *model.Task, includeOwnOutput bool, errMin, errMax bool) {
		must := make(map[string]bool)
		for _, id := range reqMap[s.ID] {
			must[id] = true
		}
		if includeOwnOutput && taskHasOutput(s) {
			must[s.ID] = true
		}
		may := make(map[string]bool)
		for id := range must {
			may[id] = true
		}
		for _, id := range optMap[s.ID] {
			may[id] = true
		}
		terminals = append(terminals, terminalEnd{task: s.ID, must: must, may: may, errMin: errMin, errMax: errMax})
	}

	for i, s := range tasks {
		isNormal := (len(s.Switch) == 0 && i == n-1) ||
			func() bool {
				for _, c := range s.Switch {
					if c.Goto == model.GotoEnd {
						return true
					}
				}
				return false
			}()
		isErrEnd := func() bool {
			for _, ec := range s.OnError {
				if ec.Goto == model.GotoEnd {
					return true
				}
			}
			return false
		}()

		if isNormal {
			addTerminal(s, true, mustErrMap[s.ID], mayErrMap[s.ID])
		}
		if isErrEnd {
			// failing task never produced output; error is always present on this path
			addTerminal(s, false, true, true)
		}
	}

	return terminals
}

// outputContextSets returns which task outputs are required/optional at the process
// output boundary, and whether `error` is required or optional there — the single
// collapsed answer, for callers that cannot use the per-terminal detail.
func outputContextSets(def *model.ProcessDefinition) (required, optional []string, errRequired, errOptional bool) {
	terminals := outputTerminals(def)
	if len(terminals) == 0 {
		return
	}

	mustAtEnd := make(map[string]bool)
	for id := range terminals[0].must {
		mustAtEnd[id] = true
	}
	for _, t := range terminals[1:] {
		for id := range mustAtEnd {
			if !t.must[id] {
				delete(mustAtEnd, id)
			}
		}
	}

	mayAtEnd := make(map[string]bool)
	for _, t := range terminals {
		for id := range t.may {
			mayAtEnd[id] = true
		}
	}

	for id := range mustAtEnd {
		required = append(required, id)
	}
	for id := range mayAtEnd {
		if !mustAtEnd[id] {
			optional = append(optional, id)
		}
	}

	allErrMin := true
	for _, t := range terminals {
		if !t.errMin {
			allErrMin = false
			break
		}
	}
	anyErrMax := false
	for _, t := range terminals {
		if t.errMax {
			anyErrMax = true
			break
		}
	}
	errRequired = allErrMin
	errOptional = anyErrMax && !allErrMin
	return
}

// buildPreds constructs the predecessor graph for the task slice.
// preds[i] lists all edges that route into task i; the process start is
// represented as predEdge{idx: -1} on task 0.
func buildPreds(tasks []*model.Task) [][]predEdge {
	n := len(tasks)
	idx := make(map[string]int, n)
	for i, s := range tasks {
		idx[s.ID] = i
	}
	preds := make([][]predEdge, n)
	preds[0] = append(preds[0], predEdge{idx: -1, rule: -1})
	for i, s := range tasks {
		addedNext := false
		for _, c := range s.Switch {
			if strings.HasPrefix(c.Goto, "$") {
				if j, ok := idx[c.Goto[1:]]; ok {
					preds[j] = append(preds[j], predEdge{idx: i, rule: -1})
				}
			} else if c.Goto == model.GotoNext && !addedNext && i+1 < n {
				preds[i+1] = append(preds[i+1], predEdge{idx: i, rule: -1})
				addedNext = true
			}
		}
		// Backward-compat: tasks with no switch fall through to the next task.
		if len(s.Switch) == 0 && i+1 < n {
			preds[i+1] = append(preds[i+1], predEdge{idx: i, rule: -1})
		}
		for r, ec := range s.OnError {
			if ec.Goto != "" && ec.Goto != model.GotoEnd {
				if j, ok := idx[ec.Goto]; ok {
					preds[j] = append(preds[j], predEdge{idx: i, isErr: true, rule: r})
				}
			}
		}
	}
	return preds
}

// checkReachability returns an error if any task cannot be reached from the
// first task via switch gotos or on_error routes.
func checkReachability(tasks []*model.Task) error {
	if len(tasks) == 0 {
		return nil
	}
	preds := buildPreds(tasks)
	reachable := make([]bool, len(tasks))
	reachable[0] = true
	for {
		changed := false
		for i, ps := range preds {
			if reachable[i] {
				continue
			}
			for _, p := range ps {
				if p.idx >= 0 && reachable[p.idx] {
					reachable[i] = true
					changed = true
					break
				}
			}
		}
		if !changed {
			break
		}
	}
	for i, s := range tasks {
		if !reachable[i] {
			return fmt.Errorf("task %q is unreachable: no switch or error handler routes to it", s.ID)
		}
	}
	return nil
}

// computeContextSets computes, for each task, which prior task outputs are
// always available (required) and which are only sometimes available (optional).
// It also returns mustErr and mayErr maps indicating whether the `error` context
// key is always / sometimes present at each task.
// errSource names one on_error rule that can have set the `error` a task reads: the task the
// rule belongs to, and its index in that task's OnError slice.
type errSource struct {
	task int
	rule int
}

func computeContextSets(tasks []*model.Task) (required, optional map[string][]string, mustErr, mayErr map[string]bool, errSrc map[string][]errSource) {
	n := len(tasks)
	required = make(map[string][]string, n)
	optional = make(map[string][]string, n)
	mustErr = make(map[string]bool, n)
	mayErr = make(map[string]bool, n)
	errSrc = make(map[string][]errSource, n)
	if n == 0 {
		return
	}

	preds := buildPreds(tasks)

	hasOutput := make([]bool, n)
	for i, s := range tasks {
		hasOutput[i] = taskHasOutput(s)
	}

	allTrue := func() []bool {
		s := make([]bool, n)
		for i := range s {
			s[i] = true
		}
		return s
	}
	allFalse := func() []bool { return make([]bool, n) }
	eq := func(a, b []bool) bool {
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	// mustOut[i][j] = task j's output is ALWAYS available when entering task i.
	// Error edges clear the failing task's own output bit. mustIn[i] is the in-set
	// (available on entry, before task i's own output) captured on the converging pass,
	// so the projection below reuses it instead of recomputing the same walk.
	mustOut := make([][]bool, n)
	mustIn := make([][]bool, n)
	for i := range mustOut {
		mustOut[i] = allTrue()
	}
	for {
		changed := false
		for i := range tasks {
			in := allTrue()
			for _, p := range preds[i] {
				if p.idx == -1 {
					in = allFalse()
					break
				}
				src := mustOut[p.idx]
				if p.isErr && hasOutput[p.idx] {
					src = append([]bool{}, mustOut[p.idx]...)
					src[p.idx] = false // failing task produced no output
				}
				for j := range in {
					in[j] = in[j] && src[j]
				}
			}
			if len(preds[i]) == 0 {
				in = allFalse()
			}
			mustIn[i] = in
			out := append([]bool{}, in...)
			if hasOutput[i] {
				out[i] = true
			}
			if !eq(mustOut[i], out) {
				mustOut[i] = out
				changed = true
			}
		}
		if !changed {
			break
		}
	}

	// mayOut[i][j] = task j's output is POSSIBLY available when entering task i.
	// mayIn[i] is captured like mustIn above.
	mayOut := make([][]bool, n)
	mayIn := make([][]bool, n)
	for i := range mayOut {
		mayOut[i] = allFalse()
	}
	for {
		changed := false
		for i := range tasks {
			in := allFalse()
			for _, p := range preds[i] {
				if p.idx == -1 {
					continue
				}
				src := mayOut[p.idx]
				if p.isErr && hasOutput[p.idx] {
					src = append([]bool{}, mayOut[p.idx]...)
					src[p.idx] = false
				}
				for j := range in {
					in[j] = in[j] || src[j]
				}
			}
			mayIn[i] = in
			out := append([]bool{}, in...)
			if hasOutput[i] {
				out[i] = true
			}
			if !eq(mayOut[i], out) {
				mayOut[i] = out
				changed = true
			}
		}
		if !changed {
			break
		}
	}

	// `error` is scoped to the task an on_error rule routes TO, and nothing further: the
	// engine drops it on every ordinary transition, so there is no propagation to chase and
	// these are local questions about one task's incoming edges. A handler that wants the
	// failure to travel projects it into its own `output`, like every other value.
	mustErrArr := make([]bool, n)
	mayErrArr := make([]bool, n)
	srcArr := make([][]errSource, n)
	for i := range tasks {
		allErr := len(preds[i]) > 0
		for _, p := range preds[i] {
			if !p.isErr {
				allErr = false
				continue
			}
			mayErrArr[i] = true
			srcArr[i] = append(srcArr[i], errSource{task: p.idx, rule: p.rule})
		}
		mustErrArr[i] = allErr
	}

	for i, s := range tasks {
		errSrc[s.ID] = srcArr[i]
	}

	for i, s := range tasks {
		for j, ss := range tasks {
			switch {
			case mustIn[i][j]:
				required[s.ID] = append(required[s.ID], ss.ID)
			case mayIn[i][j]:
				optional[s.ID] = append(optional[s.ID], ss.ID)
			}
		}

		mustErr[s.ID] = mustErrArr[i]
		mayErr[s.ID] = mayErrArr[i]
	}
	return
}
