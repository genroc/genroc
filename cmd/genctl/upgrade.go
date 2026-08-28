package main

// genctl upgrade: move every live tree of a process to another version, or the one tree an
// instance id names.
//
// There is no --dry-run: compat answers "is this change safe to deploy" over two
// documents, before anything is applied, and a per-instance rehearsal over RUNNING
// instances is stale the moment it prints -- they advance to another task and the answer
// changes. What stands in for it is the shape of the real run: one atomic transaction per
// tree, idempotent, so a partial sweep is repaired by running it again.
//
// The server moves ONE tree per call and only settles instances (paused or failed), so the
// sweep is the client's job: find the roots still on the old version, pause the running
// ones, move them, and put back the ones it paused. Doing it here rather than server-side
// keeps the endpoint a single transaction over a single tree -- a server-side sweep would
// hold one open across an unbounded number of them.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

type upgradeMove struct {
	ID          string `json:"id"`
	Process     string `json:"process"`
	Task        string `json:"task"`
	FromVersion int    `json:"from_version"`
	ToVersion   int    `json:"to_version"`
	Skipped     bool   `json:"skipped"`
	Reason      string `json:"reason"`
}

type upgradeResult struct {
	Upgraded bool          `json:"upgraded"`
	Moves    []upgradeMove `json:"moves"`
}

type instanceRow struct {
	ID       string `json:"id"`
	Process  string `json:"process"`
	Version  int    `json:"version"`
	Status   string `json:"status"`
	ParentID string `json:"parent_id"`
}

func runUpgradeCmd(server string, args []string) {
	fs := flag.NewFlagSet("upgrade", flag.ExitOnError)
	fromFlag := fs.String("from", "",
		"the version instances are on now: a number, or a channel name. Selects the sweep; instance ids need no --from")
	toFlag := fs.String("to", "", "the version to move them to: a number, or a channel name")
	statusFlag := fs.String("status", "",
		"comma-separated states to sweep: running, paused, failed. Default is all three")
	jsonOut := fs.Bool("json", false, "print one JSON object per tree instead of a progress table")
	serverFlag := addServerFlag(fs, server)
	pos := leadingArgs(fs, args)

	ids := 0
	for _, p := range pos {
		if isInstanceRef(p) {
			ids++
		}
	}
	if len(pos) == 0 || (ids > 0 && ids != len(pos)) {
		// A list that mixes the two would sweep one process and move the named trees, which
		// no summary line can report as one number.
		fatal("usage: genctl upgrade <process> --from <version|channel> --to <version|channel>\n" +
			"       genctl upgrade <instance-id> [<instance-id> ...] --to <version|channel>")
	}
	if ids > 0 {
		upgradeByIDs(*serverFlag, pos, *fromFlag, *toFlag, *statusFlag, *jsonOut)
		return
	}
	if len(pos) != 1 {
		fatal("a sweep takes one process, and %d were named; to move several trees, name their ids", len(pos))
	}
	process := pos[0]
	if *fromFlag == "" || *toFlag == "" {
		fatal("--from and --to are both required: an upgrade that names one side hides which instances it would move")
	}
	want := parseSweepStatuses(*statusFlag)
	from := resolveVersionRef(*serverFlag, process, *fromFlag)
	to := resolveVersionRef(*serverFlag, process, *toFlag)
	if from == to {
		fatal("--from and --to both resolve to version %d; nothing to move", from)
	}

	// Roots only, and only those still on the old version. A child is not a unit of upgrade
	// -- its root moves the whole tree -- so a sweep that included them would collect
	// refusals it could do nothing about. Roots-only is the endpoint's default now, which
	// is why nothing here asks for it.
	base := appendQuery(*serverFlag+"/api/instances", "process", process)
	base = appendQuery(base, "version", strconv.Itoa(from))

	var tally upgradeTally
	err := streamPages(base, func(rows []instanceRow) error {
		for _, row := range rows {
			if !want[row.Status] {
				continue
			}
			tally.record(row, upgradeOneTree(*serverFlag, row, to, *jsonOut), *jsonOut)
		}
		return nil
	})
	if err != nil {
		fatal("%v", err)
	}
	tally.done(fmt.Sprintf(" from %d to %d", from, to), *jsonOut)
}

// upgradeTally counts what became of each tree and prints the refusals as they happen, so a
// reason names its instance whether the trees came from a sweep or from a list of ids.
type upgradeTally struct{ moved, skipped, refused int }

func (t *upgradeTally) record(row instanceRow, res *string, jsonOut bool) {
	switch {
	case res == nil:
		t.moved++
	case *res == "":
		t.skipped++
	default:
		t.refused++
		if !jsonOut {
			fmt.Printf("%-38s %-16s REFUSED  %s\n", row.ID, row.Process, *res)
		}
	}
}

func (t upgradeTally) done(target string, jsonOut bool) {
	if !jsonOut {
		fmt.Fprintf(os.Stderr, "\nmoved %d tree(s)%s", t.moved, target)
		if t.skipped > 0 {
			fmt.Fprintf(os.Stderr, ", %d already there", t.skipped)
		}
		if t.refused > 0 {
			fmt.Fprintf(os.Stderr, ", %d refused", t.refused)
		}
		fmt.Fprintln(os.Stderr)
	}
	if t.refused > 0 {
		// A refusal is the answer, not a crash -- but the exit code has to carry it, or a
		// script sweeping a fleet reports success while instances stayed behind.
		os.Exit(1)
	}
}

// isInstanceRef reports whether the positional is shaped like an instance reference: a
// UUID (idgen mints v7), or the @last sigil the rest of the CLI already reads as one.
// Serves two callers. `upgrade` and `compat` use it to tell an id from a PROCESS NAME,
// which is never written either way; the lifecycle commands use it to reject an argument
// that cannot name a row at all, before anything is sent (instanceIDsAndFlags).
func isInstanceRef(arg string) bool {
	if arg == "@last" {
		return true
	}
	_, err := uuid.Parse(arg)
	return err == nil
}

// upgradeByIDs moves the trees the ids name, one atomic call each. --from is the sweep's
// SELECTOR, so ids -- which select already -- do not need it: each version is read off its own
// row and goes out as that write's assertion. specs/version-compatibility.md s6.
func upgradeByIDs(server string, refs []string, fromRef, toRef, statusFlag string, jsonOut bool) {
	if statusFlag != "" {
		fatal("--status narrows a sweep; instance ids already name the trees that move")
	}
	if toRef == "" {
		fatal("--to is required: the version to move the named tree(s) to")
	}
	var tally upgradeTally
	for _, ref := range refs {
		id := resolveInstanceID(ref)
		// detail rather than the status shape, for parent_id: the sweep never sees a child
		// (?root=true), and here the refusal has to come before the pause mutates a row the
		// server would then refuse anyway.
		var row instanceRow
		if err := callGet(server+"/api/instances/"+id+"/detail", &row); err != nil {
			tally.record(instanceRow{ID: id}, reasonf("%v", err), jsonOut)
			continue
		}
		tally.record(row, upgradeNamedTree(server, row, fromRef, toRef, jsonOut), jsonOut)
	}
	tally.done(" to "+toRef, jsonOut)
}

// upgradeNamedTree answers what only the row can -- it is a root, it is where the caller
// believes it is, and it is not there already -- then moves it. One id refused must not stop
// the ones after it, so every answer here is a reason rather than an exit.
func upgradeNamedTree(server string, row instanceRow, fromRef, toRef string, jsonOut bool) *string {
	if row.ParentID != "" {
		return reasonf("has a parent (%s); upgrade its root instead, which moves the whole tree", row.ParentID)
	}
	if fromRef != "" {
		from, err := lookupVersionRef(server, row.Process, fromRef)
		if err != nil {
			return reasonf("%v", err)
		}
		if from != row.Version {
			return reasonf("--from resolves to version %d, but this is on %d", from, row.Version)
		}
	}
	to, err := lookupVersionRef(server, row.Process, toRef)
	if err != nil {
		return reasonf("%v", err)
	}
	if to == row.Version {
		// Not a refusal: naming the same ids again after a partial run must repair it and
		// exit 0, which is the shape this command stands on instead of a --dry-run.
		reportAlreadyThere(row, to, jsonOut)
		return reasonf("")
	}
	if !movableStatuses()[row.Status] {
		return reasonf("status is %s; an upgrade moves running, paused or failed instances -- "+
			"completed and raised move no work, and failing/pausing are still draining", row.Status)
	}
	return upgradeOneTree(server, row, to, jsonOut)
}

// reportAlreadyThere keeps --json one object per named tree. A tree that needs no call still
// gets one, marshalled from the struct the moved ones print -- which is the client's shape
// either way, since upgradeOneTree prints what it DECODED and not the server's bytes.
func reportAlreadyThere(row instanceRow, to int, jsonOut bool) {
	if !jsonOut {
		fmt.Printf("%-38s %-16s already on %d\n", row.ID, row.Process, to)
		return
	}
	b, _ := json.Marshal(upgradeResult{Moves: []upgradeMove{{
		ID: row.ID, Process: row.Process, FromVersion: row.Version, ToVersion: to, Skipped: true,
	}}})
	fmt.Println(string(b))
}

// upgradeOneTree pauses a running root, moves it, and puts it back if it paused it. Returns
// nil when it moved, a pointer to "" when there was nothing to do, and a pointer to the reason
// when it did not -- which the caller reports, so a pause that failed is not counted in silence.
func upgradeOneTree(server string, row instanceRow, to int, jsonOut bool) *string {
	paused := false
	if row.Status == "running" {
		// The endpoint only moves settled rows, so a running one is paused first. A pause is
		// a request, not an act: it lands on the owner's next write, so this waits for it.
		if err := call(server+"/api/instances/"+row.ID+"/pause", "POST", nil, nil); err != nil {
			return reasonf("pause: %v", err)
		}
		if err := waitForStatus(server, row.ID, "paused", 10*time.Second); err != nil {
			return reasonf("%v", err)
		}
		paused = true
	}

	var res upgradeResult
	err := call(server+"/api/instances/"+row.ID+"/upgrade", "POST",
		map[string]any{"from_version": row.Version, "to_version": to}, &res)

	// Put it back before reporting anything: an instance this command paused must not be
	// left paused because the move failed.
	if paused {
		if rerr := call(server+"/api/instances/"+row.ID+"/resume", "POST", nil, nil); rerr != nil {
			fmt.Fprintf(os.Stderr, "genctl: %s moved but could not be resumed: %v\n", row.ID, rerr)
		}
	}
	if err != nil {
		return reasonf("%v", err)
	}

	if jsonOut {
		b, _ := json.Marshal(res)
		fmt.Println(string(b))
	}
	for _, m := range res.Moves {
		if m.Reason != "" {
			// The member that blocked the tree, not the root: on a child refusal the root's own
			// row says nothing about why.
			if m.ID != row.ID {
				return reasonf("%s (%s): %s", m.Process, m.ID, m.Reason)
			}
			return &m.Reason
		}
	}
	if !jsonOut {
		fmt.Printf("%-38s %-16s -> %d (%d in tree)\n", row.ID, row.Process, to, len(res.Moves))
	}
	return nil
}

// parseSweepStatuses reads --status. The default is every state that can actually move:
// completed and raised move no work at all -- an upgrade would only re-lens data they
// already hold -- and failing/pausing are mid-drain, with descendants still running, so the
// server refuses them. Naming one of those is a mistake worth reporting rather than a
// filter that silently matches nothing.
func parseSweepStatuses(flag string) map[string]bool {
	movable := movableStatuses()
	if flag == "" {
		return movable
	}
	want := map[string]bool{}
	for _, s := range strings.Split(flag, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !movable[s] {
			fatal("--status %q: an upgrade moves running, paused or failed instances; "+
				"completed and raised move no work, and failing/pausing are still draining", s)
		}
		want[s] = true
	}
	if len(want) == 0 {
		fatal("--status names no states")
	}
	return want
}

// movableStatuses is the set an upgrade can act on -- the states the server settles from,
// plus running, which the client pauses first.
func movableStatuses() map[string]bool {
	return map[string]bool{"running": true, "paused": true, "failed": true}
}

func reasonf(format string, a ...any) *string {
	s := fmt.Sprintf(format, a...)
	return &s
}

// waitForStatus polls until the instance settles. A pause is a request the engine lands on
// its owner's next write, so there is no synchronous form to wait on.
func waitForStatus(server, id, want string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var row instanceRow
		if err := callGet(server+"/api/instances/"+id, &row); err != nil {
			return fmt.Errorf("read %s: %w", id, err)
		}
		if row.Status == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not reach %s within %s (still %s)", id, want, timeout, row.Status)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// resolveVersionRef reads a --from/--to side for the sweep, where either side failing to
// resolve leaves nothing to do.
func resolveVersionRef(server, process, ref string) int {
	n, err := lookupVersionRef(server, process, ref)
	if err != nil {
		fatal("%v", err)
	}
	return n
}

// lookupVersionRef reads one side: a number is a version, anything else is a channel name
// resolved against this process. Separate from the fatal above because ids name rows of
// different processes, where a channel missing on one must refuse that row, not the command.
func lookupVersionRef(server, process, ref string) (int, error) {
	if n, err := strconv.Atoi(ref); err == nil {
		return n, nil
	}
	var page struct {
		Items []struct {
			Channel string `json:"channel"`
			Version int    `json:"version"`
		} `json:"items"`
	}
	if err := callGet(appendQuery(server+"/api/channels", "name", process), &page); err != nil {
		return 0, fmt.Errorf("resolve %q for %s: %w", ref, process, err)
	}
	for _, c := range page.Items {
		if c.Channel == ref {
			return c.Version, nil
		}
	}
	return 0, fmt.Errorf("process %s has no channel %q", process, ref)
}
