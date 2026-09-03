package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Workspace membership, read from Cloud Identity because a Google ID token carries none.
// The stub answers the shape the live API's discovery document declares:
// SearchTransitiveGroupsResponse{ memberships[].groupKey.id, nextPageToken }.

func stubDirectory(t *testing.T, h http.HandlerFunc) *googleDirectory {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &googleDirectory{endpoint: srv.URL, client: srv.Client()}
}

func TestGoogleGroups_ReadsTheGroupAddresses(t *testing.T) {
	var gotQuery, gotAuth string
	d := stubDirectory(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery, gotAuth = r.URL.Query().Get("query"), r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(map[string]any{"memberships": []map[string]any{
			{"groupKey": map[string]string{"id": "platform@example.com"}, "displayName": "Platform"},
			{"groupKey": map[string]string{"id": "oncall@example.com"}, "displayName": "On-call"},
		}})
	})
	got, err := d.groups(context.Background(), "ya29.token", "ada@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "platform@example.com,oncall@example.com" {
		t.Errorf("groups = %v; the group's EMAIL is what a role map keys on, because a display "+
			"name is neither unique nor stable", got)
	}
	// The API refuses a query that does not name both the member and a label.
	if !strings.Contains(gotQuery, "member_key_id == 'ada@example.com'") ||
		!strings.Contains(gotQuery, "in labels") {
		t.Errorf("query = %q; Cloud Identity requires a member specification AND a label", gotQuery)
	}
	if gotAuth != "Bearer ya29.token" {
		t.Errorf("auth = %q; the call goes out as the PERSON, which is what avoids a service "+
			"account and a stored Google credential", gotAuth)
	}
}

func TestGoogleGroups_FollowsPagesButNotForever(t *testing.T) {
	calls := 0
	d := stubDirectory(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		// Always a next page: an unbounded walk would turn one login into unbounded API calls.
		json.NewEncoder(w).Encode(map[string]any{
			"memberships":   []map[string]any{{"groupKey": map[string]string{"id": "g@example.com"}}},
			"nextPageToken": "more",
		})
	})
	got, err := d.groups(context.Background(), "t", "ada@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if calls != googleGroupsMaxPages {
		t.Errorf("made %d calls, want the cap of %d", calls, googleGroupsMaxPages)
	}
	if len(got) != googleGroupsMaxPages {
		t.Errorf("kept %d groups across %d pages", len(got), calls)
	}
}

// A refusal must not read as "this person has no groups": that would sign them in with fewer
// permissions than they have, which looks like a broken role map and is debugged as one.
func TestGoogleGroups_AnErrorIsAnErrorNotAnEmptyList(t *testing.T) {
	d := stubDirectory(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":{"message":"Request had insufficient authentication scopes."}}`))
	})
	got, err := d.groups(context.Background(), "t", "ada@example.com")
	if err == nil {
		t.Fatalf("a 403 came back as %v with no error", got)
	}
	// The operator has to be able to tell an unconsented scope from an outage.
	if !strings.Contains(err.Error(), "insufficient authentication scopes") {
		t.Errorf("error loses what Google said: %v", err)
	}
}

func TestGoogleGroups_NoAccessTokenIsRefusedBeforeTheCall(t *testing.T) {
	d := stubDirectory(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("called the API with no access token")
	})
	if _, err := d.groups(context.Background(), "", "ada@example.com"); err == nil {
		t.Error("an empty access token was sent as a request")
	}
}
