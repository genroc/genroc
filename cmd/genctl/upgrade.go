package main

// genctl upgrade: move every live tree of a process to another version.
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
	ID      string `json:"id"`
	Process string `json:"process"`
	Version int    `json:"version"`
	Status  string `json:"status"`
}

func runUpgradeCmd(server string, args []string) {
	fs := flag.NewFlagSet("upgrade", flag.ExitOnError)
	fromFlag := fs.String("from", "", "the version instances are on now: a number, or a channel name")
	toFlag := fs.String("to", "", "the version to move them to: a number, or a channel name")
	statusFlag := fs.String("status", "",
		"comma-separated states to sweep: running, paused, failed. Default is all three")
	jsonOut := fs.Bool("json", false, "print one JSON object per tree instead of a progress table")
	serverFlag := addServerFlag(fs, server)
	pos := leadingArgs(fs, args)

	if len(pos) != 1 {
		fatal("usage: genctl upgrade <process> --from <version|channel> --to <version|channel>")
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
	// refusals it could do nothing about.
	base := appendQuery(*serverFlag+"/instances", "process", process)
	base = appendQuery(base, "version", strconv.Itoa(from))
	base = appendQuery(base, "root", "true")

	var moved, skipped, refused int
	err := streamPages(base, func(rows []instanceRow) error {
		for _, row := range rows {
			if !want[row.Status] {
				continue
			}
			ok := upgradeOneTree(*serverFlag, row, to, *jsonOut)
			switch {
			case ok == nil:
				moved++
			case *ok == "":
				skipped++
			default:
				refused++
			}
		}
		return nil
	})
	if err != nil {
		fatal("%v", err)
	}

	if !*jsonOut {
		fmt.Fprintf(os.Stderr, "\nmoved %d tree(s) from %d to %d", moved, from, to)
		if skipped > 0 {
			fmt.Fprintf(os.Stderr, ", %d already there", skipped)
		}
		if refused > 0 {
			fmt.Fprintf(os.Stderr, ", %d refused", refused)
		}
		fmt.Fprintln(os.Stderr)
	}
	if refused > 0 {
		// A refusal is the answer, not a crash -- but the exit code has to carry it, or a
		// script sweeping a fleet reports success while instances stayed behind.
		os.Exit(1)
	}
}

// upgradeOneTree pauses a running root, moves it, and puts it back if it paused it.
// Returns nil when it moved, a pointer to "" when there was nothing to do, and a pointer to
// the reason when the server refused.
func upgradeOneTree(server string, row instanceRow, to int, jsonOut bool) *string {
	paused := false
	if row.Status == "running" {
		// The endpoint only moves settled rows, so a running one is paused first. A pause is
		// a request, not an act: it lands on the owner's next write, so this waits for it.
		if err := call(server+"/instances/"+row.ID+"/pause", "POST", nil, nil); err != nil {
			return reasonf("pause: %v", err)
		}
		if err := waitForStatus(server, row.ID, "paused", 10*time.Second); err != nil {
			return reasonf("%v", err)
		}
		paused = true
	}

	var res upgradeResult
	err := call(server+"/instances/"+row.ID+"/upgrade", "POST",
		map[string]any{"from_version": row.Version, "to_version": to}, &res)

	// Put it back before reporting anything: an instance this command paused must not be
	// left paused because the move failed.
	if paused {
		if rerr := call(server+"/instances/"+row.ID+"/resume", "POST", nil, nil); rerr != nil {
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
			if !jsonOut {
				fmt.Printf("%-38s %-16s REFUSED  %s\n", row.ID, m.Process, m.Reason)
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
	movable := map[string]bool{"running": true, "paused": true, "failed": true}
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
		if err := callGet(server+"/instances/"+id, &row); err != nil {
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

// resolveVersionRef reads a --from/--to side: a number is a version, anything else is a
// channel name resolved against this process.
func resolveVersionRef(server, process, ref string) int {
	if n, err := strconv.Atoi(ref); err == nil {
		return n
	}
	var page struct {
		Items []struct {
			Channel string `json:"channel"`
			Version int    `json:"version"`
		} `json:"items"`
	}
	if err := callGet(appendQuery(server+"/channels", "name", process), &page); err != nil {
		fatal("resolve %q for %s: %v", ref, process, err)
	}
	for _, c := range page.Items {
		if c.Channel == ref {
			return c.Version
		}
	}
	fatal("process %s has no channel %q", process, ref)
	return 0
}
