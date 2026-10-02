package main

// genctl upgrade: move every live tree of a process, or the trees named by id, to another version.
// No --dry-run (specs/version-compatibility.md); the sweep is client-side, one atomic idempotent
// call per tree, never one server transaction over many (specs/id-list-commands.md).

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
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
	fs := newFlagSet("upgrade", args)
	fromFlag := fs.String("from", "",
		"the version to move from: a number or a channel")
	toFlag := fs.String("to", "", "the version to move to: a number or a channel")
	statusFlag := fs.String("status", "",
		"statuses to move, comma-separated: running, paused, failed (default all three)")
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

	// Roots only -- the endpoint's default, so nothing asks for it; a child is not a unit of
	// upgrade -- and only those still on the old version.
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
			fmt.Printf("%-10s %-12s REFUSED  %s\n", row.ID, row.Process, *res)
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

// The leading DIGIT keeps a process name from matching: `upgrade` and `compat` read a name and an
// id in the same positional, and `catcher` is otherwise a perfectly good id.
var (
	mintedIDRe = regexp.MustCompile(`^[0-9][0-9a-hjkmnp-tv-z]{7,13}$`)
	legacyIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// isInstanceRef tells an id from a PROCESS NAME (upgrade, compat) and refuses a non-id before
// anything is sent (instanceIDsAndFlags).
func isInstanceRef(arg string) bool {
	return arg == "@last" || mintedIDRe.MatchString(arg) || legacyIDRe.MatchString(arg)
}

// upgradeByIDs needs no --from: ids already select, and each row's own version goes out as that
// write's assertion. specs/version-compatibility.md s6.
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
		// detail, for parent_id: a child must be refused before the pause mutates a row the
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

// upgradeNamedTree returns a reason, never exits: one refused id must not stop the ones after it.
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

// reportAlreadyThere keeps --json one object per named tree, marshalled from the struct
// upgradeOneTree prints (what it DECODED, not the server's bytes).
func reportAlreadyThere(row instanceRow, to int, jsonOut bool) {
	if !jsonOut {
		fmt.Printf("%-10s %-12s already on %d\n", row.ID, row.Process, to)
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
		// The endpoint only moves settled rows, so a running one is paused first.
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
		fmt.Printf("%-10s %-12s -> %d (%d in tree)\n", row.ID, row.Process, to, len(res.Moves))
	}
	return nil
}

// parseSweepStatuses refuses an unmovable status rather than letting it filter to nothing:
// completed/raised move no work, and failing/pausing are mid-drain, which the server refuses.
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

// waitForStatus polls: a pause lands on its owner's next write, so there is no synchronous form.
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

func resolveVersionRef(server, process, ref string) int {
	n, err := lookupVersionRef(server, process, ref)
	if err != nil {
		fatal("%v", err)
	}
	return n
}

// lookupVersionRef does not exit: ids name rows of different processes, and a channel missing
// on one must refuse that row, not the command.
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
