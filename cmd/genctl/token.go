package main

// genctl token: API credentials managed over the API, so it needs an admin credential; the
// break-glass `genroc token` needs none. specs/api-auth.md §5.3.

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

// maskToken shows enough to tell two credentials apart and not enough to use one. The prefix
// is not secret — it is the same on every token — so only the tail is elided.
func maskToken(t string) string {
	const keep = 6
	if len(t) <= len(tokenPrefix)+keep {
		return "…"
	}
	return t[:len(tokenPrefix)+keep] + "…"
}

// tokenPrefix mirrors db.TokenPrefix: genctl may not import internal/db.
const tokenPrefix = "genroc_sk_"

func runTokenCmd(server string, args []string) {
	if len(args) == 0 {
		missingSubcommand("token")
	}
	if hasHelpArg(args[:1]) {
		helpFor("token")
		return
	}
	sub, rest := args[0], args[1:]

	fs := newFlagSet("token "+sub, rest)
	serverFlag := addServerFlag(fs, server)
	labelFlag := fs.String("label", "", "a name for this token, shown in listings")
	permsFlag := fs.String("perms", "", "comma-separated: admin, deploy, operate, read, worker")
	jsonFlag := fs.Bool("json", false, "print the raw items as a JSON array")
	quietFlag := fs.Bool("quiet", false, "on create, print only the token")
	fs.BoolVar(quietFlag, "q", false, "shorthand for --quiet")
	fs.Parse(rest)

	switch sub {
	case "generate":
		// Offline: `create` needs an authenticated server, so it cannot make the FIRST token.
		// The secret never originates inside genroc.
		secret, err := generateTokenSecret()
		if err != nil {
			fatal("%v", err)
		}
		fmt.Println(secret)
	case "create":
		if *permsFlag == "" {
			fatal("token create: --perms is required (e.g. --perms deploy,read)")
		}
		var perms []string
		for _, p := range strings.Split(*permsFlag, ",") {
			if p = strings.TrimSpace(p); p != "" {
				perms = append(perms, p)
			}
		}
		var resp struct {
			ID    string   `json:"id"`
			Token string   `json:"token"`
			Perms []string `json:"perms"`
		}
		body := map[string]any{"label": *labelFlag, "perms": perms}
		if err := call(*serverFlag+"/api/tokens", http.MethodPost, body, &resp); err != nil {
			fatal("%v", err)
		}
		if *quietFlag {
			fmt.Println(resp.Token)
			return
		}
		fmt.Fprintf(os.Stderr, "created %s  perms=%s\n  shown once:\n", resp.ID, strings.Join(resp.Perms, ","))
		fmt.Println(resp.Token)
	case "list":
		// Decoded once as raw items so --json echoes the server verbatim, then per item for
		// the table. Two fields sharing a `json:"items"` tag would silently decode to nothing.
		var page struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := callGet(*serverFlag+"/api/tokens", &page); err != nil {
			fatal("%v", err)
		}
		if *jsonFlag {
			printJSONItems(page.Items)
			return
		}
		if len(page.Items) == 0 {
			fmt.Println("no tokens")
			return
		}
		type tokenRow struct {
			ID         string   `json:"id"`
			Label      string   `json:"label"`
			Perms      []string `json:"perms"`
			CreatedAt  string   `json:"created_at"`
			LastUsedAt string   `json:"last_used_at"`
			RevokedAt  string   `json:"revoked_at"`
			ExpiresAt  string   `json:"expires_at"`
			Actor      string   `json:"actor"`
			RevokedBy  string   `json:"revoked_by"`
		}
		rows := make([]tokenRow, 0, len(page.Items))
		for _, raw := range page.Items {
			var t tokenRow
			if err := json.Unmarshal(raw, &t); err != nil {
				fatal("decode token: %v", err)
			}
			rows = append(rows, t)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tLABEL\tPERMS\tACTOR\tCREATED\tLAST USED\tEXPIRES\tSTATUS")
		now := time.Now().UTC()
		for _, t := range rows {
			// Expiry is a status: a lapsed token shown "live" is skipped while chasing a 401.
			status := "live"
			switch {
			case t.RevokedAt != "":
				status = "revoked"
			case t.ExpiresAt != "":
				if at, err := time.Parse(time.RFC3339, t.ExpiresAt); err == nil && !at.After(now) {
					status = "expired"
				}
			}
			// Once revoked, the revoker displaces the minter: the newer fact.
			who := t.Actor
			if t.RevokedBy != "" {
				who = t.RevokedBy
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, orDash(t.Label),
				strings.Join(t.Perms, ","), orDash(who), shortTime(t.CreatedAt),
				orDash(shortTime(t.LastUsedAt)), orDash(shortTime(t.ExpiresAt)), status)
		}
		w.Flush()
	case "revoke":
		if fs.NArg() == 0 {
			fatal("usage: genctl token revoke <id>...")
		}
		for _, id := range fs.Args() {
			var resp struct {
				Revoked bool `json:"revoked"`
			}
			if err := call(*serverFlag+"/api/tokens/"+url.PathEscape(id), http.MethodDelete, nil, &resp); err != nil {
				fatal("%v", err)
			}
			fmt.Printf("revoked: %s\n", id)
		}
	default:
		fatal("token: unknown subcommand %q (create, list, revoke)", sub)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// generateTokenSecret mirrors db.NewTokenSecret (no internal/db import). A convenience: the
// server validates the format regardless (db.ValidateTokenSecret).
func generateTokenSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}
