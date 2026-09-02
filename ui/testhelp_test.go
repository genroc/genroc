package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newJar() http.CookieJar {
	j, err := cookiejar.New(nil)
	if err != nil {
		panic(err)
	}
	return j
}
