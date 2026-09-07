package dbtest

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	dbpkg "genroc/internal/db"
	"genroc/internal/model"
)

// TestCancelledIsClosedToEveryVerb is the "and nothing else" half of the guarantee. Each verb
// is closed for its OWN reason and in its own package, so the rule only holds as long as every
// one of them keeps holding it -- and a verb added later is exactly what this catches. What is
// asserted is not the shape of each refusal but the one thing they must share: the status is
// still 'cancelled' afterwards.
func TestCancelledIsClosedToEveryVerb(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()

			// Parked on an external task and claimed, so the external verbs have a real handle
			// to be refused with rather than failing on a missing row.
			insertExternalParked(t, b.db, "stopped", 0, nil)
			claimed, err := b.db.ClaimExternalTasks("w1", claimLease, 1, "", 0, "")
			if err != nil || len(claimed) != 1 {
				t.Fatalf("claim: got %d err=%v", len(claimed), err)
			}
			taskEpoch, claimEpoch := claimed[0].TaskEpoch, claimed[0].ExternalClaimEpoch

			if _, err := b.db.CancelProcess(ctx, "stopped", ""); err != nil {
				t.Fatalf("CancelProcess: %v", err)
			}

			verbs := []struct {
				name string
				call func() error
			}{
				{"pause", func() error { _, err := b.db.PauseProcess(ctx, "stopped", ""); return err }},
				{"resume", func() error { _, err := b.db.ResumeProcess(ctx, "stopped", ""); return err }},
				{"retry", func() error { _, err := b.db.RetryProcess(ctx, "stopped", false, ""); return err }},
				{"retry --force", func() error { _, err := b.db.RetryProcess(ctx, "stopped", true, ""); return err }},
				{"cancel again", func() error { _, err := b.db.CancelProcess(ctx, "stopped", ""); return err }},
				{"resolve", func() error {
					return b.db.ResolveExternalTask(ctx, "stopped", taskEpoch,
						dbpkg.BoundToClaim(claimEpoch), model.ExternalOutcome{Result: map[string]any{"ok": true}})
				}},
				{"signal", func() error {
					_, err := b.db.DeliverSignal(ctx, "stopped", "approval",
						model.ExternalOutcome{Result: map[string]any{"ok": true}})
					return err
				}},
				{"engine claim", func() error {
					got, err := b.db.ClaimInstances("w2", time.Minute, 10, dbpkg.AllowTakeover())
					if err != nil {
						return err
					}
					for _, c := range got {
						if c.ID == "stopped" {
							return fmt.Errorf("the engine claimed a cancelled instance")
						}
					}
					return nil
				}},
				{"external claim", func() error {
					got, err := b.db.ClaimExternalTasks("w3", claimLease, 10, "", 0, "")
					if err != nil {
						return err
					}
					for _, c := range got {
						if c.ID == "stopped" {
							return fmt.Errorf("a worker claimed a cancelled instance's task")
						}
					}
					return nil
				}},
			}

			for _, v := range verbs {
				// The error itself is not the assertion: some of these refuse and some are
				// assertions that report a no-op, and both are correct answers. What none of
				// them may do is move the row.
				if err := v.call(); err != nil && strings.Contains(err.Error(), "cancelled instance") {
					t.Errorf("%s: %v", v.name, err)
				}
				got, err := b.db.GetInstance("stopped")
				if err != nil {
					t.Fatalf("%s: GetInstance: %v", v.name, err)
				}
				if got.Status != model.StatusCancelled {
					t.Fatalf("%s moved a cancelled instance to %q; a cancel is final", v.name, got.Status)
				}
			}

			// Release is the ONE thing a cancelled row still accepts, and it must: the
			// heartbeat tells a cancelled worker to release, so refusing here would make that
			// instruction impossible to carry out.
			if err := b.db.ReleaseExternalClaim(ctx, "stopped", taskEpoch, claimEpoch); err != nil {
				t.Errorf("release must still work on a cancelled row: %v", err)
			}
			if got, _ := b.db.GetInstance("stopped"); got.Status != model.StatusCancelled {
				t.Errorf("release moved the row to %q", got.Status)
			}
		})
	}
}
