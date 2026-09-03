package main

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// The login endpoints. The PAGE is a React bundle built alongside the app (frontend/login.html);
// this serves it and answers the two calls it makes. specs/ui-issued-tokens.md §5.

// options tells the login page which ways in exist. Unauthenticated, and has to be: nothing has
// a credential at this point. It discloses only what the buttons would announce anyway.
func (s *uiServer) options(w http.ResponseWriter, r *http.Request) {
	type provider struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	out := struct {
		Providers []provider `json:"providers"`
		Passwords bool       `json:"passwords"`
	}{Providers: []provider{}, Passwords: len(s.cfg.Login.Passwords) > 0}
	for _, p := range s.order {
		name := p.Name
		if name == "" {
			name = p.ID
		}
		out.Providers = append(out.Providers, provider{ID: p.ID, Name: name})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(out)
}

// serveLoginPage hands over the login bundle. It is a different document from the app's, not a
// route inside it: the app lives behind the session this page exists to create.
func (s *uiServer) serveLoginPage(w http.ResponseWriter, r *http.Request) {
	page, err := fs.ReadFile(s.assets, "login.html")
	if err != nil {
		http.Error(w, "login page is not built into this binary", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Never cached: it reflects whether a session exists, and a cached copy would show a login
	// page to someone already signed in.
	w.Header().Set("Cache-Control", "no-store")
	w.Write(page)
}

// passwordLogin checks a bcrypt hash from the config. specs/ui-issued-tokens.md §5 -- the
// `staticPasswords` trade: one file, no directory, no registration, no reset.
//
// It answers JSON rather than re-rendering, because the caller is the login bundle's `fetch`.
// The session cookie rides on this response and the page then navigates, which is what makes
// the browser pick it up.
func (s *uiServer) passwordLogin(w http.ResponseWriter, r *http.Request) {
	if s.sign == nil || len(s.cfg.Login.Passwords) == 0 {
		writeJSONError(w, http.StatusNotImplemented, "password login is not configured")
		return
	}
	var req struct{ Email, Password string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad request")
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	addr := clientIP(r)

	// Checked BEFORE the hash comparison, so a throttled attacker costs nothing to refuse. The
	// message does not say which limit tripped: telling an attacker whether they hit the
	// per-email or per-address budget tells them whether the address exists.
	for _, c := range []struct {
		key string
		max int
	}{{"email:" + email, maxEmailFailures}, {"addr:" + addr, maxAddrFailures}} {
		if ok, retry := s.limiter.allow(c.key, c.max); !ok {
			s.log.Warn("password login throttled", "email", email, "addr", addr,
				"retry_after", retry.Round(time.Second))
			w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
			writeJSONError(w, http.StatusTooManyRequests,
				"Too many failed attempts. Try again later.")
			return
		}
	}

	// One message for every failure, and the hash is compared even when no such user exists:
	// distinguishing "no such account" from "wrong password" tells an attacker which addresses
	// are real, and returning early on a miss says the same thing through timing.
	var found *Password
	for i := range s.cfg.Login.Passwords {
		if strings.EqualFold(s.cfg.Login.Passwords[i].Email, email) {
			found = &s.cfg.Login.Passwords[i]
			break
		}
	}
	hash := placeholderHash
	if found != nil {
		hash = found.Hash
	}
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password))
	if found == nil || err != nil {
		// Both keys, always: counting only the email lets an attacker spray many accounts from
		// one address and never trip anything.
		s.limiter.fail("email:" + email)
		s.limiter.fail("addr:" + addr)
		s.log.Warn("password login failed", "email", email, "addr", addr)
		writeJSONError(w, http.StatusUnauthorized, "That email and password did not match.")
		return
	}
	// A correct password says the earlier misses were a person mistyping. The ADDRESS budget is
	// deliberately not cleared: one success must not buy an attacker a fresh budget for the
	// other accounts they are working through.
	s.limiter.succeed("email:" + email)
	s.log.Info("password login", "email", found.Email)
	if err := s.setSession(w, r, identity{Subject: found.Email, Groups: found.Groups}); err != nil {
		s.log.Error("mint session", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// 204: the cookie is the whole answer, and the page navigates itself.
	w.WriteHeader(http.StatusNoContent)
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// placeholderHash is a real bcrypt hash of a value nobody knows, compared against when the email
// is unknown so that the work done is the same either way.
const placeholderHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"
