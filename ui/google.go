package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Workspace groups, fetched once at login with the person's own access token, then dropped.
// Cloud Identity, not the Admin SDK, whose scope is RESTRICTED. TRANSITIVE so nested groups
// inherit (`searchDirectGroups` answers INVALID_ARGUMENT to this query).

const (
	googleGroupsScope   = "https://www.googleapis.com/auth/cloud-identity.groups.readonly"
	googleGroupsAPI     = "https://cloudidentity.googleapis.com/v1/groups/-/memberships:searchTransitiveGroups"
	googleGroupsTimeout = 10 * time.Second
	// The label every Workspace group carries. The API requires the query to name one, so this
	// is not a filter we chose -- it is what makes the query legal.
	googleGroupsLabel = "cloudidentity.googleapis.com/groups.discussion_forum"
	// A person in more groups than this is not going to be told apart by the next page, and an
	// unbounded walk turns one login into an unbounded number of API calls.
	googleGroupsMaxPages = 5
)

type googleDirectory struct {
	endpoint string // overridden in tests; the live API otherwise
	client   *http.Client
}

func newGoogleDirectory() *googleDirectory {
	return &googleDirectory{
		endpoint: googleGroupsAPI,
		client:   &http.Client{Timeout: googleGroupsTimeout},
	}
}

// groups returns group EMAILS, which role maps key on: a display name is neither unique nor
// stable.
func (g *googleDirectory) groups(ctx context.Context, accessToken, subject string) ([]string, error) {
	if accessToken == "" {
		return nil, fmt.Errorf("no access token from the exchange; cloud-identity groups need one")
	}
	var out []string
	pageToken := ""
	for page := 0; page < googleGroupsMaxPages; page++ {
		q := url.Values{
			// CEL single quotes. A Google-issued subject email cannot contain one; escaped anyway.
			"query":    {fmt.Sprintf("member_key_id == '%s' && '%s' in labels", escapeCEL(subject), googleGroupsLabel)},
			"pageSize": {"200"},
		}
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.endpoint+"?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		resp, err := g.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("cloud-identity groups: %w", err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("cloud-identity groups: %s: %s", resp.Status,
				strings.TrimSpace(string(body)))
		}
		var page struct {
			Memberships []struct {
				GroupKey struct {
					ID string `json:"id"`
				} `json:"groupKey"`
			} `json:"memberships"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("cloud-identity groups: %w", err)
		}
		for _, m := range page.Memberships {
			if id := strings.TrimSpace(m.GroupKey.ID); id != "" {
				out = append(out, id)
			}
		}
		if page.NextPageToken == "" {
			return out, nil
		}
		pageToken = page.NextPageToken
	}
	return out, nil
}

func escapeCEL(s string) string {
	return strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s)
}
