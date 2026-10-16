package dbtest

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	dbpkg "genroc/internal/db"
	"genroc/internal/model"
)

// A model check of the drain (specs/pause-resume.md §2-§4): a random tree, every held leaf's
// outcome and a few operator verbs racing each other, then a simulated engine claims and settles
// until nothing is claimable. Whatever the interleaving, nothing may be left draining or busy, the
// root must report the whole tree, and the verb that recovers it must finish it.

type drainTree struct {
	root     string
	mids     []string
	leaves   []string
	parentOf map[string]string
}

type leafOp int

const (
	opComplete   leafOp = iota // the task concludes the child
	opFail                     // a defect
	opRaise                    // a declared outcome: terminal, not a failure
	opCheckpoint               // the task ends and the next one is due: a 'running' save
	opCrash                    // the worker dies holding the row; its lease lapses
	leafOps
)

const drainLease = time.Minute

func buildDrainTree(t *testing.T, db *dbpkg.DB, rng *rand.Rand, tag string) drainTree {
	t.Helper()
	tr := drainTree{root: tag + "root", parentOf: map[string]string{}}
	insertInstW(t, db, tr.root, model.StatusRunning, model.PhaseChildren, "", nil, "")
	for i := 0; i < 2+rng.Intn(2); i++ {
		mid := fmt.Sprintf("%sm%d", tag, i)
		insertChildW(t, db, mid, model.StatusRunning, model.PhaseChildren, tr.root, []string{tr.root})
		tr.mids = append(tr.mids, mid)
		tr.parentOf[mid] = tr.root
		for j := 0; j < 1+rng.Intn(3); j++ {
			leaf := fmt.Sprintf("%sm%dl%d", tag, i, j)
			insertChild(t, db, leaf, model.StatusRunning, mid, "step1", []string{tr.root, mid}, "")
			tr.leaves = append(tr.leaves, leaf)
			tr.parentOf[leaf] = mid
		}
	}
	return tr
}

// save is how the engine persists a stopped row (saveAndNotify).
func save(db *dbpkg.DB, inst *model.ProcessInstance) error {
	if inst.Status == model.StatusFailed && inst.ParentID != "" {
		return db.FailInstanceAndAncestors(inst)
	}
	return db.FinishChild(inst)
}

// drainSim plays the engine: settle what drains, complete what runs. It stops at quiescence.
// window, when set, runs between a claim and its write: where a verb meets a settle in flight.
func drainSim(t *testing.T, db *dbpkg.DB, label string, window func()) {
	t.Helper()
	lapsed := false
	for round := 0; round < 50; round++ {
		claimed, err := db.ClaimInstances("sim", drainLease, 100, dbpkg.AllowTakeover())
		if err != nil {
			t.Fatalf("%s: ClaimInstances: %v", label, err)
		}
		if len(claimed) == 0 {
			if lapsed {
				return
			}
			// A crashed holder's row is reclaimable only once its lease is gone.
			advance(t, 2*drainLease)
			lapsed = true
			continue
		}
		lapsed = false
		for _, inst := range claimed {
			if window != nil {
				window()
			}
			switch inst.Status {
			case model.StatusPausing:
				inst.Status = model.StatusPaused
			case model.StatusCancelling:
				inst.Status = model.StatusCancelled
			case model.StatusRunning:
				inst.Status = model.StatusCompleted
			default:
				t.Errorf("%s: %s claimed as %q; the settle-up should have moved it", label, inst.ID, inst.Status)
				continue
			}
			if err := save(db, inst); err != nil {
				t.Fatalf("%s: settling %s: %v", label, inst.ID, err)
			}
		}
	}
	t.Fatalf("%s: the simulated engine never went quiet", label)
}

// checkDrained asserts the end state every interleaving must reach, and returns the root's status.
func checkDrained(t *testing.T, db *dbpkg.DB, tr drainTree, failCode map[string]string, label string) model.Status {
	t.Helper()
	all := append(append([]string{tr.root}, tr.mids...), tr.leaves...)
	st := map[string]*model.ProcessInstance{}
	for _, id := range all {
		inst, err := db.GetInstance(id)
		if err != nil {
			t.Fatalf("%s: GetInstance %s: %v", label, id, err)
		}
		st[id] = inst
		if inst.Status.Busy() {
			t.Errorf("%s: %s is %q at quiescence: a drain was lost and nothing is left to land it", label, id, inst.Status)
		}
	}
	root := st[tr.root].Status
	switch root {
	case model.StatusCompleted, model.StatusFailed, model.StatusPaused, model.StatusCancelled:
	default:
		t.Errorf("%s: root is %q at quiescence", label, root)
	}

	anyFailed, anyPaused := false, false
	for _, id := range all {
		switch st[id].Status {
		case model.StatusFailed:
			anyFailed = true
		case model.StatusPaused:
			anyPaused = true
		}
	}
	if anyFailed && root != model.StatusFailed && root != model.StatusCancelled {
		t.Errorf("%s: a row failed but the root reports %q", label, root)
	}
	if anyPaused && root != model.StatusPaused && root != model.StatusFailed {
		t.Errorf("%s: paused rows under a %q root", label, root)
	}
	if root == model.StatusCompleted {
		for _, id := range all {
			if s := st[id].Status; s != model.StatusCompleted && s != model.StatusRaised {
				t.Errorf("%s: %s is %q under a completed root", label, id, s)
			}
		}
	}
	// Every failed ancestor reports a failure from its own subtree: FailAncestors touches only the
	// failing row's call stack, and the first failure's report is never overwritten.
	for _, id := range append([]string{tr.root}, tr.mids...) {
		if st[id].Status != model.StatusFailed {
			continue
		}
		ok := false
		for leaf, code := range failCode {
			if (tr.parentOf[leaf] == id || id == tr.root) && st[id].ErrorCode == code {
				ok = true
			}
		}
		if !ok {
			t.Errorf("%s: %s reports %q, which no failure below it raised", label, id, st[id].ErrorCode)
		}
	}
	return root
}

func TestDrainModel_RandomInterleavings(t *testing.T) {
	const iterations = 40
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			for i := 0; i < iterations; i++ {
				seed := time.Now().UnixNano() + int64(i)
				rng := rand.New(rand.NewSource(seed))
				label := fmt.Sprintf("seed %d", seed)
				tr := buildDrainTree(t, b.db, rng, fmt.Sprintf("i%d-", i))

				// Every leaf is claimed; a random share is handed back at once, so the race has
				// idle rows (which stop in the verb's statement) beside held ones (which drain).
				claimed, err := b.db.ClaimInstances("w", drainLease, 100, dbpkg.AllowTakeover())
				if err != nil {
					t.Fatalf("%s: ClaimInstances: %v", label, err)
				}
				var held []*model.ProcessInstance
				for _, inst := range claimed {
					if rng.Intn(3) == 0 {
						if err := b.db.UpdateInstance(inst); err != nil {
							t.Fatalf("%s: release %s: %v", label, inst.ID, err)
						}
						continue
					}
					held = append(held, inst)
				}

				failCode := map[string]string{}
				var mu sync.Mutex
				var wg sync.WaitGroup
				start := make(chan struct{})
				for _, inst := range held {
					op := leafOp(rng.Intn(int(leafOps)))
					wg.Add(1)
					go func(inst *model.ProcessInstance, op leafOp) {
						defer wg.Done()
						<-start
						switch op {
						case opComplete:
							inst.Status = model.StatusCompleted
						case opFail:
							code := "boom_" + inst.ID[len(inst.ID)-4:]
							mu.Lock()
							failCode[inst.ID] = code
							mu.Unlock()
							inst.Status, inst.ErrorCode, inst.ErrorMessage = model.StatusFailed, code, code
						case opRaise:
							inst.Status, inst.ErrorCode = model.StatusRaised, "declined"
						case opCheckpoint:
							if err := b.db.UpdateInstance(inst); err != nil {
								t.Errorf("%s: checkpoint %s: %v", label, inst.ID, err)
							}
							return
						case opCrash:
							return
						}
						if err := save(b.db, inst); err != nil {
							if pgDeadlock(err) {
								t.Errorf("%s: %s: %v", label, deadlockHint, err)
							} else {
								t.Errorf("%s: %s %v: %v", label, inst.ID, inst.Status, err)
							}
						}
					}(inst, op)
				}
				for v := 0; v < rng.Intn(4); v++ {
					vrng := rand.New(rand.NewSource(rng.Int63()))
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						randomVerb(t, b.db, vrng, tr.root, label)
					}()
				}
				close(start)
				wg.Wait()

				// The verbs also land inside the settles, but only a few, so the run still ends.
				inWindow := 0
				drainSim(t, b.db, label, func() {
					if inWindow < 3 && rng.Intn(4) == 0 {
						inWindow++
						randomVerb(t, b.db, rng, tr.root, label)
					}
				})
				switch checkDrained(t, b.db, tr, failCode, label) {
				case model.StatusFailed:
					if _, err := b.db.RetryProcess(ctx, tr.root, false, ""); err != nil {
						t.Fatalf("%s: RetryProcess: %v", label, err)
					}
				case model.StatusPaused:
					if _, err := b.db.ResumeProcess(ctx, tr.root, ""); err != nil {
						t.Fatalf("%s: ResumeProcess: %v", label, err)
					}
				default:
					continue
				}
				// Retry revives every failure and resumes what it paused; resume flips the rest. With
				// no new failures either way, the tree must now run to completion.
				drainSim(t, b.db, label+" (recovered)", nil)
				if got := checkDrained(t, b.db, tr, nil, label+" (recovered)"); got != model.StatusCompleted {
					t.Errorf("%s: after recovery the root is %q, want completed", label, got)
				}
				if t.Failed() {
					return
				}
			}
		})
	}
}

// randomVerb is one operator verb on the tree. A resume of a failed or failing tree is refused by
// design, so a conflict is an answer, not a failure.
func randomVerb(t *testing.T, db *dbpkg.DB, rng *rand.Rand, root, label string) {
	ctx := context.Background()
	var err error
	switch rng.Intn(3) {
	case 0:
		_, err = db.PauseProcess(ctx, root, "")
	case 1:
		_, err = db.ResumeProcess(ctx, root, "")
	default:
		_, err = db.CancelProcess(ctx, root, "")
	}
	if err != nil && !errors.Is(err, dbpkg.ErrConflict) {
		t.Errorf("%s: verb: %v", label, err)
	}
}
