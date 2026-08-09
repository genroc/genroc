package schema

import (
	"fmt"
	"sort"
)

// The Solver resolves a system of named definitions whose bodies are computed
// rather than given: each Declare'd name maps to a closure that infers its
// schema (e.g. a task's output map inferred against a context). Resolution is
// demand-driven — when a computation looks inside a `$ref` to a definition that
// has not been computed yet, the solver computes it right there, so definitions
// are solved in exact dependency order without a separately-maintained graph.
//
// Cycles are detected on contact: re-entering a definition that is currently
// being computed collapses the demand-stack segment between the two into a
// cluster (the strongly-connected component, discovered Gabow-style), which is
// then resolved by a joint fixpoint: every member seeded null, re-computed,
// joined and canonicalized until stable — the same semantics the previous
// per-SCC fixpoint had, now driven by the refs the computations actually
// follow. While a member is mid-computation, readers inside the cycle see its
// running estimate wrapped nullable (the null seed before the first pass) —
// nullability lives at the use site; the finalized definition is the exact
// type. See specs/recursive-type-inference.md.
type Solver struct {
	defs    Defs
	members map[string]*solverMember
	stack   []*solverMember
	epoch   int
}

// maxSolvePasses bounds the joint fixpoint over one cluster. Fixed-shape
// accumulators (counters, sums, toggles) converge in 1–2 passes; the cap turns
// a genuinely diverging type into an error instead of an infinite loop.
const maxSolvePasses = 16

// Widening bound on a solved type's canonical size: a non-converging recursion grows
// exponentially per pass, so the pass cap alone would build megabytes before giving up.
// Far larger than any real output type; catches divergence within a few passes.
const maxSolvedTypeBytes = 64 * 1024

// pendingAnchor marks a solver sentinel node. Sentinels never appear in solved
// output; the marker exists so a leaked sentinel fails loudly in deref instead
// of validating as an empty (permissive) schema.
const pendingAnchor = "genroc:pending"

type pendingEntry struct {
	solver *Solver
	name   string
}

type memberState uint8

const (
	memberUnstarted memberState = iota
	memberOnStack
	memberDone
)

type solverMember struct {
	name     string
	compute  func() (Schema, error)
	sentinel *node
	state    memberState
	stackPos int
	cluster  *solverCluster
	est      Schema
	hasEst   bool
	final    *node
	err      error // poison: a failed member keeps failing on later reads
}

// solverCluster is a detected cycle (strongly-connected set) of members. It is
// always a contiguous segment at the top of the demand stack: merges take the
// whole segment from the re-entered member up. version changes whenever
// membership grows, so a running fixpoint notices and restarts.
type solverCluster struct {
	members map[string]*solverMember
	version int
}

func (c *solverCluster) sorted() []*solverMember {
	out := make([]*solverMember, 0, len(c.members))
	for _, m := range c.members {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func (c *solverCluster) names() []string {
	out := make([]string, 0, len(c.members))
	for n := range c.members {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// NewSolver returns a solver writing into the given definitions handle, which must be
// live (NewDefs or a schema's own defs): declared names get a pending sentinel
// immediately and their solved types afterwards, observed by every schema sharing it.
func NewSolver(defs Defs) *Solver {
	if defs.m == nil {
		// A zero handle has no backing map to publish into; that is a
		// programming error, not an input condition.
		panic("schema.NewSolver: defs handle must be live (use NewDefs)")
	}
	return &Solver{defs: defs, members: map[string]*solverMember{}}
}

// Declare registers a definition to be solved, replacing any existing entry under name
// with a pending sentinel until Solve resolves it. Declaring the same name twice is a
// programming error.
func (s *Solver) Declare(name string, compute func() (Schema, error)) {
	if _, dup := s.members[name]; dup {
		panic(fmt.Sprintf("schema.Solver: definition %q declared twice", name))
	}
	sentinel := &node{Anchor: pendingAnchor, ID: name, pending: &pendingEntry{solver: s, name: name}}
	m := &solverMember{name: name, compute: compute, sentinel: sentinel}
	s.members[name] = m
	s.defs.m[name] = sentinel
}

// Solve computes every declared definition in exact dependency order (a computation
// reading `$ref <other>` pulls <other> first), resolving cycles with a joint fixpoint
// per cluster. On success every declared name holds its solved type in the defs handle;
// on error the pending entries are unusable and the caller should discard the generation.
func (s *Solver) Solve() error {
	defer func() {
		for _, m := range s.members {
			m.sentinel.pending = nil
		}
	}()
	names := make([]string, 0, len(s.members))
	for n := range s.members {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		m := s.members[n]
		if m.state == memberUnstarted {
			if err := s.solveMember(m); err != nil {
				return err
			}
		}
		// Collapse any degenerate cycle completed so far, so later members
		// reading these defs see the collapsed (concrete) form.
		if err := s.collapseDegenerateCycles(); err != nil {
			return err
		}
	}
	// Everything left cyclic is productive recursion — a legal recursive type.
	// A failure here is a solver bug, not an input condition.
	if err := checkProductivity(s.defs.m); err != nil {
		return fmt.Errorf("internal: solved definitions failed the productivity check: %w", err)
	}
	return nil
}

// Collapses definition cycles with no structural progress (every edge a bare $ref in
// union position): X = anyOf[X, I] says only X = I, so members collapse to the cycle's
// non-cyclic remainders; no remainder = no base case = error. Property/items cycles are
// real recursion and stay; cycles holding a pending sentinel wait for a later call.
func (s *Solver) collapseDegenerateCycles() error {
	// Bare-edge graph over the current defs (see collectBareRefs).
	defs := s.defs.m
	graph := make(map[string][]string, len(defs))
	names := make([]string, 0, len(defs))
	for name, d := range defs {
		names = append(names, name)
		set := map[string]struct{}{}
		collectBareRefs(d, set)
		edges := make([]string, 0, len(set))
		for e := range set {
			if _, ok := defs[e]; ok {
				edges = append(edges, e)
			}
		}
		sort.Strings(edges)
		graph[name] = edges
	}
	sort.Strings(names)

	for _, scc := range tarjanSCC(graph, names) {
		selfEdge := false
		if len(scc) == 1 {
			for _, e := range graph[scc[0]] {
				if e == scc[0] {
					selfEdge = true
				}
			}
			if !selfEdge {
				continue // not a cycle
			}
		}
		sort.Strings(scc)
		inSCC := make(map[string]bool, len(scc))
		for _, n := range scc {
			inSCC[n] = true
		}
		// A cycle through a definition this solver does not own cannot be
		// rewritten here; surface it like CheckDoc would.
		for _, n := range scc {
			if s.members[n] == nil {
				return fmt.Errorf("$defs cycle without structural progress: %v (recursion must pass through properties or items)", scc)
			}
		}
		var combined Schema
		has := false
		for _, n := range scc {
			rest := dropBareSCCRefs(defs[n], inSCC)
			if rest == nil {
				continue
			}
			if !has {
				combined, has = Schema{rest}, true
			} else {
				combined = combined.Join(Schema{rest})
			}
		}
		if !has {
			return fmt.Errorf("output type for %q is defined only in terms of itself — recursion with no base case (add a non-recursive arm, e.g. ?? <default>)", scc[0])
		}
		final := canonicalizeNode(combined.n)
		for _, n := range scc {
			defs[n] = final
			if m := s.members[n]; m != nil && m.state == memberDone {
				m.final = final
			}
		}
	}
	return nil
}

// dropBareSCCRefs returns n with every bare-position $ref into the SCC removed from its
// unions, or nil when nothing else remains at this position. Properties/items subtrees
// are untouched — a ref there is productive and stays.
func dropBareSCCRefs(n *node, scc map[string]bool) *node {
	if n == nil {
		return nil
	}
	const prefix = "#/$defs/"
	if len(n.Ref) > len(prefix) && n.Ref[:len(prefix)] == prefix && scc[n.Ref[len(prefix):]] {
		return nil
	}
	m := mapChildren(n, func(sl childSlot, c *node) *node {
		if sl.kind != slotBare {
			return c // a ref below properties/items is productive and stays
		}
		return dropBareSCCRefs(c, scc)
	})
	hadVariants := len(n.OneOf)+len(n.AnyOf)+len(n.AllOf) > 0
	hasVariants := len(m.OneOf)+len(m.AnyOf)+len(m.AllOf) > 0
	if hadVariants && !hasVariants && len(m.Type) == 0 && m.Properties == nil &&
		m.AdditionalProperties == nil && m.Items == nil && m.Ref == "" && m.Enum == nil {
		return nil // the node was purely its (now dropped) union
	}
	return m
}

// tarjanSCC returns the strongly-connected components of graph in dependency-first
// (reverse-topological) order. nodes fixes iteration order for determinism.
func tarjanSCC(graph map[string][]string, nodes []string) [][]string {
	index := make(map[string]int, len(nodes))
	low := make(map[string]int, len(nodes))
	onStack := make(map[string]bool, len(nodes))
	var stack []string
	next := 0
	var sccs [][]string

	var strongconnect func(v string)
	strongconnect = func(v string) {
		index[v] = next
		low[v] = next
		next++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range graph[v] {
			if _, seen := index[w]; !seen {
				strongconnect(w)
				low[v] = min(low[v], low[w])
			} else if onStack[w] {
				low[v] = min(low[v], index[w])
			}
		}
		if low[v] == index[v] {
			var scc []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				scc = append(scc, w)
				if w == v {
					break
				}
			}
			sccs = append(sccs, scc)
		}
	}

	for _, v := range nodes {
		if _, seen := index[v]; !seen {
			strongconnect(v)
		}
	}
	return sccs
}

// resolvePending is called from deref when a `$ref` lands on a pending sentinel; it
// returns the node the reader should see — the final type once solved, or the running
// (nullable) estimate when the read closes a cycle.
func (s *Solver) resolvePending(name string) (*node, error) {
	m := s.members[name]
	if m == nil {
		return nil, fmt.Errorf("internal: pending definition %q has no member", name)
	}
	if m.err != nil {
		return nil, m.err
	}
	switch m.state {
	case memberDone:
		return m.final, nil
	case memberOnStack:
		// Back-edge: the demand chain from m up to the top of the stack is a
		// cycle — collapse it into one cluster and serve the estimate.
		s.mergeFrom(m.stackPos)
		return m.estimateNode(), nil
	default:
		if err := s.solveMember(m); err != nil {
			return nil, err
		}
		if m.state == memberDone {
			return m.final, nil
		}
		// m joined a cluster owned by a frame below us; serve its estimate.
		return m.estimateNode(), nil
	}
}

// estimateNode serves the member's running estimate to a reader inside the cycle: the
// null seed before the first fixpoint pass (which lets a `?? default` base case fire),
// then the nullable-wrapped estimate. Nullability is expressed here at the use site —
// the definition itself is stored as the exact type.
func (m *solverMember) estimateNode() *node {
	if !m.hasEst {
		return &node{Type: SchemaType{"null"}}
	}
	return withNull(m.est.n)
}

// solveMember runs a member's first computation. With no cycle the result is final
// (everything it read was solved on demand first). If it landed in a cluster, the
// discovery result is discarded and the cluster root drives the joint fixpoint instead —
// every member from the same null seed, as if the cycle had been known upfront.
func (s *Solver) solveMember(m *solverMember) error {
	m.state = memberOnStack
	m.stackPos = len(s.stack)
	s.stack = append(s.stack, m)

	res, err := m.compute()
	if err != nil {
		m.err = s.wrapMemberErr(m, err)
		return m.err
	}

	if m.cluster == nil {
		res = res.Canonicalize()
		if res.Size() > maxSolvedTypeBytes {
			m.err = s.grewError(m)
			return m.err
		}
		s.finalize(m, res)
		s.stack = s.stack[:len(s.stack)-1]
		return nil
	}
	if s.clusterRoot(m.cluster) != m {
		// A frame below us owns the cluster; stay on the stack (back-edges to
		// us must keep merging) and let the root's fixpoint compute us.
		return nil
	}
	return s.solveCluster(m)
}

// solveCluster runs the joint fixpoint for the cluster rooted at self. The cluster may
// grow mid-pass (a member's computation can demand a new definition that reaches back
// in); growth restarts the fixpoint from fresh seeds. Growth is monotone and bounded by
// the declared-member count, so the restarts terminate.
func (s *Solver) solveCluster(self *solverMember) error {
	for {
		c := self.cluster
		if s.clusterRoot(c) != self {
			// The cluster grew below us; the new root's frame resolves it.
			return nil
		}
		version := c.version
		for _, m := range c.sorted() {
			m.est, m.hasEst = Schema{}, false
		}
		stable := false
		for pass := 0; pass < maxSolvePasses && !stable; pass++ {
			stable = true
			for _, m := range c.sorted() {
				cur, err := m.compute()
				if err != nil {
					m.err = s.wrapMemberErr(m, err)
					return m.err
				}
				if self.cluster != c || c.version != version {
					break
				}
				cur = cur.Canonicalize()
				if cur.Size() > maxSolvedTypeBytes {
					m.err = s.grewError(m)
					return m.err
				}
				next := cur
				if m.hasEst {
					next = m.est.Join(cur)
					if !next.Equal(m.est) {
						stable = false
					}
				} else {
					stable = false
				}
				m.est, m.hasEst = next, true
			}
			if self.cluster != c || c.version != version {
				break
			}
		}
		if self.cluster != c || c.version != version {
			continue // membership changed — restart with the enlarged cluster
		}
		if !stable {
			return fmt.Errorf("recursive output types did not stabilize after %d passes (cycle: %v)", maxSolvePasses, c.names())
		}
		for _, m := range c.sorted() {
			s.finalize(m, m.est)
		}
		s.stack = s.stack[:len(s.stack)-len(c.members)]
		return nil
	}
}

// finalize publishes a member's solved type into the defs handle and retires its
// sentinel. The stored definition is the exact type — estimate readers got their
// nullable wrapper at the use site instead.
func (s *Solver) finalize(m *solverMember, res Schema) {
	final := res.n
	if final == nil {
		final = &node{}
	}
	m.final = final
	s.defs.m[m.name] = final
	m.sentinel.pending = nil
	m.state = memberDone
	m.cluster = nil
}

// mergeFrom collapses the stack segment [pos..top] — plus any clusters its members
// already belong to — into a single cluster. Re-merging an existing cluster is a no-op
// (no version bump), so intra-cluster reads during a pass do not restart it.
func (s *Solver) mergeFrom(pos int) {
	segment := s.stack[pos:]
	if len(segment) == 0 {
		return
	}
	merged := map[string]*solverMember{}
	for _, m := range segment {
		if m.cluster != nil {
			for _, cm := range m.cluster.members {
				merged[cm.name] = cm
			}
		} else {
			merged[m.name] = m
		}
	}
	if first := segment[0].cluster; first != nil && len(first.members) == len(merged) {
		same := true
		for n := range merged {
			if first.members[n] == nil {
				same = false
				break
			}
		}
		if same {
			return
		}
	}
	s.epoch++
	c := &solverCluster{members: merged, version: s.epoch}
	for _, m := range merged {
		m.cluster = c
	}
}

func (s *Solver) clusterRoot(c *solverCluster) *solverMember {
	var root *solverMember
	for _, m := range c.members {
		if root == nil || m.stackPos < root.stackPos {
			root = m
		}
	}
	return root
}

// wrapMemberErr attributes a computation error: a top-level definition's error is
// returned untouched; one failing while demanded by another is prefixed with its name so
// the demand chain is visible.
func (s *Solver) wrapMemberErr(m *solverMember, err error) error {
	if m.stackPos == 0 {
		return err
	}
	return fmt.Errorf("%s: %w", m.name, err)
}

func (s *Solver) grewError(m *solverMember) error {
	return fmt.Errorf("output type for %q grew past %d bytes without converging — likely an unbounded recursion (e.g. accumulating self.previous without a base case)", m.name, maxSolvedTypeBytes)
}
