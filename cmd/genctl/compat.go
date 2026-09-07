package main

// genctl compat: compare two sides of a deployment and report what a version move would
// break, plus the whole rendering of that report. specs/compat-command.md.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

// parseSelector turns one --from/--to's repeated values into the API selector: EITHER one
// channel OR name@<version|channel> entries. Mixing is refused, not merged — a channel
// already names a version for every process, so an extra entry is ambiguous.
func parseSelector(side string, values []string) map[string]any {
	channels, pins := 0, map[string]any{}
	for _, v := range values {
		name, ref, ok := strings.Cut(v, "@")
		if !ok {
			channels++
			continue
		}
		if name == "" || ref == "" {
			fatal("--%s %q: expected name@version or name@channel", side, v)
		}
		if _, dup := pins[name]; dup {
			// Silently keeping one would compare a version the user did not choose.
			fatal("--%s names %q twice; a side carries one version per process", side, name)
		}
		if n, err := strconv.Atoi(ref); err == nil {
			pins[name] = n
		} else {
			pins[name] = ref
		}
	}
	switch {
	case channels > 0 && len(pins) > 0:
		fatal("--%s mixes a channel with name@version entries; a side is one or the other", side)
	case channels > 1:
		fatal("--%s names more than one channel; a side carries one version per process", side)
	case channels == 1:
		return map[string]any{"channel": values[0]}
	case len(pins) > 0:
		return map[string]any{"versions": pins}
	}
	return nil
}

// compatSidesForInstance reads both sides off a row: the side it RUNS is its own process at
// its own version, so only the target is named. The row's process also scopes the report --
// a --to channel carries every process on it. specs/version-compatibility.md s6.
func compatSidesForInstance(server, id string, fromFlag, toFlag, files multiFlag) (map[string]any, map[string]any, string) {
	if len(fromFlag) > 0 {
		fatal("an instance id already names the side it is running; drop --from")
	}
	if len(files) > 0 && len(toFlag) > 0 {
		fatal("-f already names the target side; drop --to")
	}
	var row instanceRow
	if err := callGet(server+"/api/instances/"+id, &row); err != nil {
		fatal("%v", err)
	}
	from := map[string]any{"versions": map[string]any{row.Process: row.Version}}
	switch {
	case len(files) > 0:
		defs, err := resolvedDefs(files)
		if err != nil {
			fatal("%v", err)
		}
		return from, map[string]any{"definitions": defs}, row.Process
	case len(toFlag) > 1:
		fatal("--to names %d targets; after an instance id it names one version or channel for %s",
			len(toFlag), row.Process)
	case len(toFlag) == 1:
		if n, err := strconv.Atoi(toFlag[0]); err == nil {
			return from, map[string]any{"versions": map[string]any{row.Process: n}}, row.Process
		}
		return from, map[string]any{"channel": toFlag[0]}, row.Process
	}
	fatal("usage: genctl compat <instance-id> --to <version|channel> | genctl compat <instance-id> -f <file>")
	return nil, nil, ""
}

func runCompatCmd(server string, args []string) {
	fs := newFlagSet("compat", args)
	var fromFlag, toFlag multiFlag
	fs.String("f", "", "definition file or glob to compare against --from; takes several, and repeats")
	processFlag := fs.String("process", "", "narrow the report to one process")
	fs.Var(&fromFlag, "from", "the side instances are running now: a channel, or name@version (repeatable). "+
		"An instance id names this side by itself")
	fs.Var(&toFlag, "to", "the side to compare against: a channel, or name@version (repeatable); "+
		"after an instance id, a bare version or channel")
	serverFlag := addServerFlag(fs, server)
	jsonFlag := fs.Bool("json", false, "print the raw report")
	var ignore multiFlag
	fs.Var(&ignore, "ignore", "excuse a check from the exit code: only `contract` is accepted, since the "+
		"upgrade check answers for rows this deployment already owns. It changes neither what is "+
		"compared nor what is printed")
	files, rest := takeFileValues(args)
	pos := leadingArgs(fs, rest)

	for _, p := range pos {
		// compat's positions are SELECTORS, so a path here is a mistake with a quiet failure
		// mode: it would be taken as a process name, match nothing, and report an empty
		// comparison with exit 0. An unquoted `-f defs/*.yaml` expands to exactly this.
		if looksLikePath(p) {
			fatal("%s looks like a file, and compat's positional arguments are selectors.\n"+
				"Pass files with -f (repeatable), or one quoted pattern: -f 'definitions/*.genroc.yaml'", p)
		}
		if isInstanceRef(p) && len(pos) > 1 {
			// A side carries one version per process (parseSelector's rule), and a second row is
			// a second version -- of the same process, or of one this report is not scoped to.
			fatal("compat takes one instance id: two rows are two comparisons, and a side carries " +
				"one version per process")
		}
	}

	// `compat --from latest` with nothing else: the local project IS the target side. This is
	// the question worth asking before an apply -- does what I have here break what is running?
	// Only when no other side was named, so it cannot hijack a stored-versus-stored comparison.
	if len(files) == 0 && len(fromFlag) > 0 && len(toFlag) == 0 && len(pos) == 0 {
		expanded, err := expandPaths(defaultDefinitionPaths("."))
		if err != nil {
			fatal("%v", err)
		}
		files = expanded
	} else if len(files) > 0 {
		expanded, err := expandFileFlags(files)
		if err != nil {
			fatal("%v", err)
		}
		files = expanded
	}

	var from, to map[string]any
	process := ""
	switch {
	case len(pos) == 1 && isInstanceRef(pos[0]):
		from, to, process = compatSidesForInstance(*serverFlag, resolveInstanceID(pos[0]), fromFlag, toFlag, files)
	case len(files) > 0:
		if len(toFlag) > 0 {
			fatal("-f already names the target side; drop --to")
		}
		if len(fromFlag) == 0 {
			fatal("--from is required with -f: naming only one side hides which two documents were compared")
		}
		// Resolved, exactly as apply resolves: an unresolved `$import:` leaf is a literal
		// string next to the code a stored version holds, so every site that has one compares
		// changed and the row can never read `unchanged`.
		defs, err := resolvedDefs(files)
		if err != nil {
			fatal("%v", err)
		}
		from, to = parseSelector("from", fromFlag), map[string]any{"definitions": defs}
	default:
		if len(fromFlag) == 0 || len(toFlag) == 0 {
			fatal("usage: genctl compat --from <sel> [-f <path|glob> ...]\n" +
				"       genctl compat --from <sel> --to <sel> [--process <name>]\n" +
				"       genctl compat <instance-id> --to <version|channel>")
		}
		from, to = parseSelector("from", fromFlag), parseSelector("to", toFlag)
		// The one positional form left is an instance id, so a name here is the dropped
		// `compat <process> <from> <to>` sugar -- which read a selector off a position and so
		// could not be told from an unquoted glob's leftovers.
		if len(pos) > 0 {
			fatal("%s: compat's only positional is an instance id. Name a process with "+
				"--process %s, and its versions with --from %s@N --to %s@M",
				pos[0], pos[0], pos[0], pos[0])
		}
	}
	// --process narrows any form. It replaced a trailing positional, which collided with an
	// unquoted `-f defs/*.yaml`: the leftover files were read as a process name, matched
	// nothing, and reported an empty comparison with exit 0.
	if *processFlag != "" {
		process = *processFlag
	}

	body := map[string]any{"from": from, "to": to}
	if process != "" {
		body["process"] = process
	}
	// Forwarded as written: the server owns the vocabulary and the gating, so a token the
	// CLI pre-validated would be a second reading to keep true.
	if len(ignore) > 0 {
		body["ignore"] = []string(ignore)
	}

	if *jsonFlag {
		var raw json.RawMessage
		if err := call(*serverFlag+"/api/definitions/compat", http.MethodPost, body, &raw); err != nil {
			fatal("%v", err)
		}
		printIndented(raw)
		// --json is a RENDERING, not a mode: it must gate exactly as the report does, or a
		// pipeline that adds it to capture the findings stops failing on them.
		var resp compatReport
		if err := json.Unmarshal(raw, &resp); err != nil {
			fatal("decode compat report: %v", err)
		}
		exitOnBreak(resp)
		return
	}

	var resp compatReport
	if err := call(*serverFlag+"/api/definitions/compat", http.MethodPost, body, &resp); err != nil {
		fatal("%v", err)
	}
	printCompatReport(resp)
	exitOnBreak(resp)
}

// The compat report as the server sends it. Nothing is parsed out of prose: a finding
// arrives addressed, because a bracket-quoted key may contain a space and no reader can
// split on it. specs/compat-command.md §6d.
type compatReport struct {
	Compatible bool            `json:"compatible"`
	Passes     bool            `json:"passes"`
	Processes  []compatProcess `json:"processes"`
}

type compatProcess struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	From   int    `json:"from"`
	To     int    `json:"to"`
	// Side and Reason are set only on an unanalysable row.
	Side   string `json:"side"`
	Reason string `json:"reason"`
	// Upgrade is what this deployment's own rows can survive; Contract is what the outside
	// world was written against. Two questions, so two columns.
	Upgrade  compatVerdict `json:"upgrade"`
	Contract compatVerdict `json:"contract"`
	Changed  []compatSlot  `json:"changed"`
	Added    []string      `json:"added"`
	Issues   []compatIssue `json:"issues"`
}

type compatVerdict struct {
	Compatible bool `json:"compatible"`
}

// compatSlot is a slot the author edited. Affects is the question it bears on; empty means
// no check covers it, which is the whole reason the channel exists.
type compatSlot struct {
	Address string   `json:"address"`
	Task    string   `json:"task"`
	Affects []string `json:"affects"`
}

// compatIssue is a value that broke: the schema that was compared, and the path isSubset
// reported inside it.
type compatIssue struct {
	Member  string `json:"member"`
	Address string `json:"address"`
	Task    string `json:"task"`
	Path    string `json:"path"`
	Message string `json:"message"`
	Gating  bool   `json:"gating"`
}

// versionLabel renders a resolved version, or "(new)" for a side carrying none — a
// submitted document, which has no version until it is applied.
func versionLabel(v int) string {
	if v == 0 {
		return "(new)"
	}
	return fmt.Sprintf("v%d", v)
}

// Verdict words. Two questions, and a word each: whether the rows this deployment owns can
// continue, and whether the process still honours what the outside world was written
// against. Folding them into one word was the defect this replaced.
const (
	verdictUpgradable = "upgradable"
	verdictCompatible = "compatible"
	verdictUnchanged  = "unchanged"
	verdictBreaking   = "breaking"
	verdictIgnored    = "ignored"
	verdictNew        = "new"
	// A version that failed its own inference was compared against nothing, so it is
	// breaking-by-default: an answer indistinguishable from "checked, and fine" is worse
	// than no report.
	verdictUnanalysable = "unanalysable"
)

// A row's status, as the server spells it. Distinct from the verdict words above even where
// the two coincide: one says whether the verdicts mean anything, the other is a verdict.
const (
	statusNothingToCompare = "nothing_to_compare"
	statusNew              = "new"
	statusUnanalysable     = "unanalysable"
)

// statusWord is the whole answer for a process carrying no verdicts, and "" for one that was
// compared. It stands for both questions because it is a property of the process rather than
// of either check — unlike a verdict, which must never speak for the member it is not (§6c).
func statusWord(status string) string {
	switch status {
	case statusNothingToCompare:
		return verdictUnchanged
	case statusNew:
		return verdictNew
	case statusUnanalysable:
		return verdictUnanalysable
	}
	return ""
}

// verdictPhrase is a process's whole answer: every member lands in exactly one fate, and none
// is ever left out — an absent member reads as a question that went unanswered (§6c).
func verdictPhrase(p compatProcess) string {
	if word := statusWord(p.Status); word != "" {
		return word
	}
	var breaking, ignored, sound []string
	for _, m := range []struct{ member, word string }{
		{"upgrade", verdictUpgradable}, {"contract", verdictCompatible},
	} {
		v := p.Contract
		if m.member == "upgrade" {
			v = p.Upgrade
		}
		switch {
		case v.Compatible:
			sound = append(sound, m.word)
		case gates(p, m.member):
			breaking = append(breaking, m.member)
		default:
			ignored = append(ignored, m.member)
		}
	}
	return fates(breaking, ignored, sound)
}

// fates is the phrasing both levels share: `,` joins members and `; ` joins fates, because
// one separator doing both jobs leaves `breaking: upgrade, contract` and `breaking: upgrade,
// ignored: contract` telling apart only by lookahead. Problems come first (§6c).
func fates(breaking, ignored, sound []string) string {
	var clauses []string
	for _, f := range []struct {
		head    string
		members []string
	}{
		{verdictBreaking + ": ", breaking}, {verdictIgnored + ": ", ignored}, {"", sound},
	} {
		if len(f.members) > 0 {
			clauses = append(clauses, f.head+strings.Join(f.members, ", "))
		}
	}
	return strings.Join(clauses, "; ")
}

// versionRange names an arrow only where two versions were compared: a row with nothing to
// compare involves ONE version, and printing a second implies a comparison that never ran.
func versionRange(p compatProcess) string {
	if p.Status == statusNothingToCompare || p.Status == statusNew {
		if p.From != 0 {
			return versionLabel(p.From)
		}
		return versionLabel(p.To)
	}
	return versionLabel(p.From) + " → " + versionLabel(p.To)
}

// pad right-pads to a RUNE count: a version range holds an arrow, and %-*s counts bytes.
func pad(s string, w int) string {
	return s + strings.Repeat(" ", w-utf8.RuneCountInString(s))
}

func gates(p compatProcess, member string) bool {
	for _, i := range p.Issues {
		if i.Member == member && i.Gating {
			return true
		}
	}
	return false
}

// printCompatReport prints each process ONCE, the verdict heading the findings it derives
// from. No header row: a block sits between two processes, and a header a screen up answers
// nothing.
func printCompatReport(r compatReport) {
	var nameW, versionW int
	for _, p := range r.Processes {
		nameW = max(nameW, utf8.RuneCountInString(p.Name))
		versionW = max(versionW, utf8.RuneCountInString(versionRange(p)))
	}

	// A blank line separates a block from the process below it and nothing else: a run of
	// processes with no findings is a list, and spacing it out hides that it is one.
	blank := false
	for _, p := range r.Processes {
		if blank {
			fmt.Println()
		}
		fmt.Printf("%s  %s  %s\n",
			pad(p.Name, nameW), pad(versionRange(p), versionW), verdictPhrase(p))
		lines := detailLines(p)
		for _, line := range lines {
			fmt.Println(line)
		}
		blank = len(lines) > 0
	}
}

// exitOnBreak is the gate, and it is deliberately not part of printing: both renderings
// answer the same question, so both must fail the same way (§6d).
func exitOnBreak(r compatReport) {
	if !r.Passes {
		os.Exit(1)
	}
}

// row is one line of the detail block: an address, the phrase saying what it costs, and the
// findings under it. A row is either a slot that changed or a place something broke, never
// both — together they would claim the edit caused the break (§6b).
type row struct {
	address string
	phrase  string
	lines   []string
}

// rowsFor walks findings first, then the changed slots — already only the ones no finding
// accounts for, the server having dropped the rest (§6b). Nothing is filtered here.
func rowsFor(p compatProcess) []row {
	var out []row
	at := map[string]int{}
	for _, i := range p.Issues {
		if _, seen := at[i.Address]; !seen {
			at[i.Address] = len(out)
			out = append(out, row{address: i.Address})
		}
		r := &out[at[i.Address]]
		line := i.Message
		if i.Path != "" {
			line = i.Path + ": " + i.Message
		}
		if !contains(r.lines, line) {
			r.lines = append(r.lines, line)
		}
	}
	// One difference that fails both questions is two findings on the wire, because they
	// gate separately — but it is one line, named for both.
	for i := range out {
		out[i].phrase = breakPhrase(p, out[i].address)
	}
	for _, s := range p.Changed {
		out = append(out, row{address: s.Address, phrase: changedPhrase(s)})
	}
	for _, task := range p.Added {
		out = append(out, row{address: task, phrase: "(added)"})
	}
	return out
}

// breakPhrase names every member that broke at this address, in the grammar the process line
// uses. Unlike that line, a row claims nothing beyond its own address, so a member that broke
// elsewhere is simply absent (§6b) — and one reads `ignored` only where EVERY finding under
// it is excused, which keeps a gating break visible under a finer selection.
func breakPhrase(p compatProcess, address string) string {
	var breaking, ignored []string
	for _, member := range []string{"upgrade", "contract"} {
		var found, gating bool
		for _, i := range p.Issues {
			if i.Address != address || i.Member != member {
				continue
			}
			found = true
			gating = gating || i.Gating
		}
		switch {
		case !found:
		case gating:
			breaking = append(breaking, member)
		default:
			ignored = append(ignored, member)
		}
	}
	return "(" + fates(breaking, ignored, nil) + ")"
}

// changedPhrase distinguishes the two things a clean change can mean, which is the only
// reason slot categories are carried at all: `ok` says a check looked and passed, `not
// judged` that none covers it. `ok` is scoped to its own address and claims nothing wider.
func changedPhrase(s compatSlot) string {
	if len(s.Affects) == 0 {
		return "(not judged)"
	}
	return "(ok)"
}

func detailLines(p compatProcess) []string {
	if p.Status == statusUnanalysable {
		return []string{fmt.Sprintf("  %s side: %s", p.Side, p.Reason)}
	}
	rows := rowsFor(p)

	// The address column is padded here rather than by a tabwriter: the finding lines
	// between two addresses carry no columns, and a tabwriter ends its alignment block at
	// every one of them — so each address would size itself and none would line up.
	width := 0
	for _, r := range rows {
		width = max(width, utf8.RuneCountInString(r.address))
	}
	var out []string
	for _, r := range rows {
		out = append(out, "  "+pad(r.address, width)+"  "+r.phrase)
		for _, line := range r.lines {
			out = append(out, "    "+line)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
