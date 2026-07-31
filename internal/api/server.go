package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"

	"genroc/internal/model"
)

// Server listens on HTTP, TCP, and/or Unix Domain Socket simultaneously.
// All three transports share the same handler logic; only the envelope extraction differs.
type Server struct {
	handlers *Handlers
	log      *slog.Logger
}

func NewServer(handlers *Handlers, log *slog.Logger) *Server {
	return &Server{handlers: handlers, log: log}
}

// ListenHTTP serves HTTP on addr until ctx is cancelled. Routes and Swagger docs are
// generated from the action registry (actions.go) — add endpoints there, not here.
func (s *Server) ListenHTTP(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	h := s.handlers

	for _, a := range registry {
		a := a
		mux.HandleFunc(a.Method+" "+a.Path, func(w http.ResponseWriter, r *http.Request) {
			env, err := a.envelope(r)
			if err != nil {
				// The envelope only fails on a body that is not JSON at all, so this
				// is always the caller's fault, never the server's.
				writeReply(w, invalid("bad request: %w", err).reply())
				return
			}
			writeReply(w, a.handle(h, env))
		})
	}

	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, swaggerUIHTML("genroc API", "/openapi.json"))
	})

	mux.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(buildSpec())
	})

	mux.HandleFunc("GET /process-schema.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(buildProcessDefinitionSchema())
	})

	mux.HandleFunc("GET /definitions/{name}/docs", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		specURL := "/definitions/" + name + "/openapi.json"
		if v := r.URL.Query().Get("version"); v != "" {
			specURL += "?version=" + v
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, swaggerUIHTML(name+" — genroc API", specURL))
	})

	mux.HandleFunc("GET /definitions/{name}/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		version := 0
		if v := r.URL.Query().Get("version"); v != "" {
			if parsed, err := strconv.Atoi(v); err == nil {
				version = parsed
			}
		}
		data, err := h.ProcessSpec(r.PathValue("name"), version)
		if err != nil {
			// Was an unconditional 404 with a text/plain body; now it classifies like
			// every other route, so a broken spec build reports 500 rather than
			// masquerading as an unknown process.
			writeReply(w, errReply(err))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	})

	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()

	s.log.Info("HTTP listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// ListenTCP serves a JSON stream over TCP: newline-delimited envelopes
// {"action":"...","payload":{...},"id":"..."}.
func (s *Server) ListenTCP(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen tcp %s: %w", addr, err)
	}
	s.log.Info("TCP listening", "addr", addr)
	return s.acceptLoop(ctx, ln)
}

func (s *Server) ListenUDS(ctx context.Context, path string) error {
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listen uds %s: %w", path, err)
	}
	s.log.Info("UDS listening", "path", path)
	return s.acceptLoop(ctx, ln)
}

func (s *Server) acceptLoop(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			// The normal shutdown path: the context goroutine above closed the
			// listener. Tested with errors.Is rather than by matching the stdlib's
			// message text — a mismatch there would turn a clean shutdown into a
			// logged error plus a hot retry loop.
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			s.log.Error("accept error", "err", err)
			continue
		}
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	for {
		var env Envelope
		if err := dec.Decode(&env); err != nil {
			return
		}
		if err := enc.Encode(s.handlers.Handle(env)); err != nil {
			s.log.Warn("write reply", "err", err)
			return
		}
	}
}

// errorBody is the JSON shape of every failed HTTP response. It mirrors the failure
// half of Reply, which is what TCP and UDS clients receive verbatim, so the three
// transports report the same facts under the same names.
type errorBody struct {
	Error  string             `json:"error"`
	Code   Code               `json:"code"`
	Fields []model.FieldError `json:"fields,omitempty"`
}

// writeReply renders a Reply over HTTP, mapping its Code to a status through the one
// table in errors.go. An unclassified failure becomes 500, not 400: an error nobody
// classified is a server problem until someone shows otherwise, and that default is
// what makes the remaining unclassified paths findable.
func writeReply(w http.ResponseWriter, r Reply) {
	w.Header().Set("Content-Type", "application/json")
	if !r.OK {
		w.WriteHeader(statusOf(r.Code))
		json.NewEncoder(w).Encode(errorBody{Error: r.Error, Code: r.Code, Fields: r.Fields})
		return
	}
	json.NewEncoder(w).Encode(r.Data)
}
