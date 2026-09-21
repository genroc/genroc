package model

// What a status means, for a reader rather than for the engine. The predicates are computed
// from the same functions the engine branches on, so a page cannot claim one thing while the
// runtime does another; only the prose is written down, and a test says it is complete.

// StatusInfo is one status as a reader meets it.
type StatusInfo struct {
	Status   Status `json:"status"`
	Terminal bool   `json:"terminal"`
	// Accepts reports whether a worker's submitted result or failure is still delivered to an
	// instance in this status — the distinction a paused tree turns on.
	Accepts bool   `json:"accepts_external_outcome"`
	Means   string `json:"means"`
}

// statusMeanings is the one line per status. `TestEveryStatusIsDocumented` reads the constants
// out of this package's own source, so a status added without a line fails here rather than
// reaching the reference as a blank cell.
var statusMeanings = map[Status]string{
	StatusRunning:    "advancing, or waiting on a timer, a child or an external task",
	StatusCompleted:  "finished by reaching the end of its definition",
	StatusFailing:    "doomed by an error, and draining the descendants still in flight",
	StatusFailed:     "stopped by an error; retryable, which is what separates it from the other settled outcomes",
	StatusRaised:     "concluded by a `raise` clause — a condition, not a defect, and catchable by the parent",
	StatusPausing:    "a pause was requested while a task was in flight; it settles at the next task boundary",
	StatusPaused:     "not being advanced, and able to resume exactly where it stopped; timers keep running",
	StatusCancelling: "a cancel was requested while a task was in flight; it settles at the next task boundary",
	StatusCancelled:  "stopped for good by an operator — terminal, and unlike a pause there is no way back",
}

// statusOrder is the order the reference lists them: live states first, then the settled
// outcomes, because that is the order an instance passes through them.
var statusOrder = []Status{
	StatusRunning, StatusPausing, StatusPaused, StatusFailing, StatusCancelling,
	StatusCompleted, StatusFailed, StatusRaised, StatusCancelled,
}

// Enum publishes the status set to the OpenAPI generator (swaggest picks up this interface),
// so the documented filter values are derived from the constants rather than copied into a
// struct tag beside them. The copy is how `cancelling` and `cancelled` went undocumented: a
// status added after the tag was written changed nothing that could fail.
func (Status) Enum() []interface{} {
	out := make([]interface{}, 0, len(statusOrder))
	for _, s := range statusOrder {
		out = append(out, s)
	}
	return out
}

// Statuses returns every status with what it means and the two predicates that separate them.
func Statuses() []StatusInfo {
	out := make([]StatusInfo, 0, len(statusOrder))
	for _, s := range statusOrder {
		out = append(out, StatusInfo{
			Status:   s,
			Terminal: s.Terminal(),
			Accepts:  s.AcceptsExternalOutcome(),
			Means:    statusMeanings[s],
		})
	}
	return out
}
