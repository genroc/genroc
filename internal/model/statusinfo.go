package model

// What a status means to a reader. The predicates come from the functions the engine branches
// on; only the prose is written down.

// StatusInfo is one status as a reader meets it.
type StatusInfo struct {
	Status   Status `json:"status"`
	Terminal bool   `json:"terminal"`
	// Accepts reports whether a worker's submitted result or failure is still delivered to an
	// instance in this status — the distinction a paused tree turns on.
	Accepts bool   `json:"accepts_external_outcome"`
	Means   string `json:"means"`
}

// statusMeanings is checked against the constants in source by TestEveryStatusIsDocumented.
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

// Enum publishes the status set to the OpenAPI generator (swaggest), derived from the constants:
// an enum copied into a struct tag is how `cancelling` and `cancelled` went undocumented.
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
