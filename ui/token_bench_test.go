package main

import (
	"testing"
	"time"
)

// What a proxied request costs beyond the proxy hop: verify the session cookie, resolve the role
// map, sign an access token. ui-issued-tokens.md §7 left "is per-request minting too expensive?"
// open; this is the measurement that answers it.

func benchSigner() *signer {
	return &signer{
		secret: []byte("a-shared-secret-long-enough-to-be-taken-seriously"),
		issuer: "genroc-ui", audience: "genroc",
		tokenTTL: time.Minute, sessionTTL: 12 * time.Hour,
	}
}

func BenchmarkPerRequest(b *testing.B) {
	s := benchSigner()
	roles := map[string][]string{"admins": {"admin"}, "*": {"read"}}
	users := map[string][]string{}
	session, _, err := s.mintSession(identity{Subject: "ada@example.com", Groups: []string{"admins"}})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id, err := s.readSession(session)
		if err != nil {
			b.Fatal(err)
		}
		perms := resolve(roles, users, id)
		if _, err := s.mintAccess(id, perms); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMintAccessOnly(b *testing.B) {
	s := benchSigner()
	id := identity{Subject: "ada@example.com"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.mintAccess(id, []string{"admin"}); err != nil {
			b.Fatal(err)
		}
	}
}
