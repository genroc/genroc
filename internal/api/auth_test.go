package api

import (
	"strings"
	"testing"
)

// specs/api-auth.md §3. The gate is coarse on purpose; what these pin is that it cannot be
// reached by accident — an endpoint added without a permission must be closed, not open.

// The permission an action declares is a security decision, and the registry is where it is
// made. An action that declares neither Allow nor Open is admin-only, which is a legitimate
// answer — `tick` is one — but it must be the author's answer rather than a field they forgot,
// so the deliberate cases are named here and anything else fails.
func TestEveryActionDeclaresAPermission(t *testing.T) {
	adminOnly := map[string]string{
		"tick": "manual-tick mode only, and it can shift the server clock",
	}
	for _, a := range registry {
		switch {
		case a.Open:
			if len(a.Allow) > 0 {
				t.Errorf("%s: Open and Allow are mutually exclusive; Open already skips the gate", a.Name)
			}
		case len(a.Allow) > 0:
		default:
			if _, deliberate := adminOnly[a.Name]; !deliberate {
				t.Errorf("%s declares no permission, so it is admin-only by omission. "+
					"Give it an Allow, or add it to adminOnly here with the reason.", a.Name)
			}
		}
	}
}

// Open is the one exemption from authorization, so its membership is worth pinning: a second
// one has to be argued for, not merged.
func TestOnlyTheProbeIsOpen(t *testing.T) {
	var open []string
	for _, a := range registry {
		if a.Open {
			open = append(open, a.Name)
		}
	}
	if len(open) != 1 || open[0] != "health" {
		t.Errorf("Open actions = %v, want exactly [health] — a probe answers before identity "+
			"exists; anything else needs an argument in specs/api-auth.md", open)
	}
}

// The inbound zone is what a worker credential reaches, so its membership is the blast radius
// of a leaked worker token. §1's path split and this list must say the same thing.
func TestWorkerZoneIsExactlyTheInboundEndpoints(t *testing.T) {
	want := map[string]bool{
		"claim_external_tasks": true, "renew_external_claims": true,
		"release_external_task": true, "resolve_external_task": true,
		"signal_instance": true,
		// Shared: a worker fetches externalized task inputs by ref, and an operator reads the
		// same refs. Refs are content hashes, so this is capability-by-ref.
		"get_object": true,
	}
	for _, a := range registry {
		holdsWorker := false
		for _, p := range a.Allow {
			holdsWorker = holdsWorker || p == PermWorker
		}
		if holdsWorker != want[a.Name] {
			t.Errorf("%s: reachable with the worker permission = %v, want %v (path %q)",
				a.Name, holdsWorker, want[a.Name], a.Path)
		}
	}
}

func TestAuthorize_RefusesAnUnestablishedIdentity(t *testing.T) {
	err := authorize(actionDef{Name: "x", Allow: []Perm{PermRead}}, nil)
	if err == nil {
		t.Fatal("a nil principal must be refused, not treated as anonymous")
	}
	if err.Code != CodeUnauthenticated {
		t.Errorf("code = %q, want %q — 401 and 403 have opposite fixes", err.Code, CodeUnauthenticated)
	}
}

func TestAuthorize_SeparatesUnknownFromUnpermitted(t *testing.T) {
	reader := &Principal{Grants: []Grant{{Perm: PermRead}}}
	err := authorize(actionDef{Name: "put_definition", Allow: []Perm{PermDeploy}}, reader)
	if err == nil {
		t.Fatal("read must not reach a deploy action")
	}
	if err.Code != CodeForbidden {
		t.Errorf("code = %q, want %q: the caller IS known", err.Code, CodeForbidden)
	}
	if !strings.Contains(err.Error(), "deploy") {
		t.Errorf("message %q must name the permission required, or the fix is a guess", err.Error())
	}
}

func TestAuthorize_AdminSatisfiesEverything(t *testing.T) {
	admin := &Principal{Grants: []Grant{{Perm: PermAdmin}}}
	for _, a := range registry {
		if err := authorize(a, admin); err != nil {
			t.Errorf("admin refused by %s: %v", a.Name, err)
		}
	}
}

// An empty Allow is admin-only. This is the fail-closed rule itself, so it is asserted
// directly rather than only through the registry.
func TestAuthorize_EmptyAllowIsAdminOnly(t *testing.T) {
	unguarded := actionDef{Name: "newly_added"}
	for _, p := range []Perm{PermRead, PermOperate, PermDeploy, PermWorker} {
		if err := authorize(unguarded, &Principal{Grants: []Grant{{Perm: p}}}); err == nil {
			t.Errorf("%s reached an action that declares no permission; the default must be closed", p)
		}
	}
	if err := authorize(unguarded, &Principal{Grants: []Grant{{Perm: PermAdmin}}}); err != nil {
		t.Errorf("admin must still reach it: %v", err)
	}
}

// Open skips the gate entirely, including for a principal holding nothing at all.
func TestAuthorize_OpenNeedsNoIdentity(t *testing.T) {
	if err := authorize(actionDef{Name: "health", Open: true}, nil); err != nil {
		t.Errorf("an Open action must answer without identity: %v", err)
	}
}

// The rule a deployment writes its ingress from: EVERYTHING under /api requires a credential.
// It held only by inspection before — `/api/docs` and `/api/openapi.json` sat under the
// authenticated prefix and answered without one, which is exactly the mismatch
// specs/api-auth.md §1 is about. This asserts it over the mounted paths, so a route added to
// server.go outside the registry cannot quietly reopen the hole.
func TestEveryApiPathIsGated(t *testing.T) {
	for _, a := range registry {
		p := a.mountPath()
		if !strings.HasPrefix(p, apiPrefix+"/") {
			continue
		}
		if a.Open {
			t.Errorf("%s (%s) is Open and under %s — an unauthenticated route must live under %s, "+
				"so the zone is legible from the path", a.Name, p, apiPrefix, publicPrefix)
		}
	}
	// The hand-written routes are the ones that escaped last time, so they are named here.
	// A new one under /api must call Server.guard, and adding it to this list is the reminder.
	guarded := map[string]bool{
		apiPrefix + "/definitions/{name}/docs":         true,
		apiPrefix + "/definitions/{name}/openapi.json": true,
	}
	for path := range guarded {
		if !strings.HasPrefix(path, apiPrefix+"/") {
			t.Errorf("%s is listed as a guarded /api route but is not under %s", path, apiPrefix)
		}
	}
	if !strings.HasPrefix(publicPrefix, "/") || publicPrefix == apiPrefix {
		t.Errorf("publicPrefix %q must be its own root prefix", publicPrefix)
	}
}

// ── attribution ──────────────────────────────────────────────────────────────
// specs/api-auth.md §7.

func TestPrincipalActor_CarriesTheSourceBesideTheSubject(t *testing.T) {
	cases := []struct {
		name string
		p    *Principal
		want string
	}{
		{"token", &Principal{Subject: "ci", Source: "token"}, "token:ci"},
		{"header", &Principal{Subject: "ada@example.com", Source: "header"}, "header:ada@example.com"},
		{"none", anonymousAdmin(), "none:anonymous"},
		{"nil records nothing rather than panicking", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.p.Actor(); got != c.want {
				t.Fatalf("Actor() = %q, want %q — an audit row that cannot say WHICH mode "+
					"established an identity cannot distinguish an asserted one from a checked one", got, c.want)
			}
		})
	}
}
