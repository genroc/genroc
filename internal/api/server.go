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
	"time"

	"genroc/internal/defschema"
	"genroc/internal/model"
)

// actorHeader echoes `source:subject` to the caller, HTTP only (CLAUDE.md).
const actorHeader = "X-Genroc-Actor"

// HTTP listener limits. Deliberately no WriteTimeout: it would sever a legitimate long
// /tick (CLAUDE.md).
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 60 * time.Second
	idleTimeout       = 120 * time.Second
	shutdownTimeout   = 15 * time.Second
	// maxRequestBytes caps every route's body, comfortably above any real definition
	// batch.
	maxRequestBytes = 10 << 20
)

// Server listens on HTTP, TCP, and/or Unix Domain Socket simultaneously.
// All three transports share the same handler logic; only the envelope extraction differs.
type Server struct {
	handlers *Handlers
	log      *slog.Logger

	// The limits above, as fields only so a test can drive them to durations it can wait
	// for. NewServer sets every one; nothing in production overrides them.
	readHeaderTimeout time.Duration
	readTimeout       time.Duration
	idleTimeout       time.Duration
	shutdownTimeout   time.Duration

	// auth establishes identity. Nil is `mode: none` — every caller is anonymousAdmin, which
	// is the pre-auth behaviour written down rather than a branch in the gate.
	auth Authenticator
}

// guard is authorize spelled at the call site, for hand-written routes that cannot answer
// with a Reply.
func (s *Server) guard(r *http.Request, allow ...Perm) *Error {
	p, err := s.httpPrincipal(r)
	if err != nil {
		return err
	}
	return authorize(actionDef{Name: r.URL.Path, Allow: allow}, p)
}

// httpPrincipal reads identity from the bearer credential only: no header, no cookie.
// specs/auth-two-credentials.md §0.
func (s *Server) httpPrincipal(r *http.Request) (*Principal, *Error) {
	return s.principalFor(r.Context(), bearerToken(r.Header.Get("Authorization")))
}

// SetAuthenticator turns on an identity mode. Called once at startup, before Listen*.
func (s *Server) SetAuthenticator(a Authenticator) { s.auth = a }

// principalFor answers 503 when an authenticator cannot DECIDE, rather than refusing a
// valid credential as invalid.
func (s *Server) principalFor(ctx context.Context, credential string) (*Principal, *Error) {
	if s.auth == nil {
		return anonymousAdmin(), nil
	}
	p, err := s.auth.Authenticate(ctx, credential)
	if err != nil {
		return nil, apiErrf(CodeUnavailable, "cannot verify credentials right now: %v", err)
	}
	return p, nil
}

func NewServer(handlers *Handlers, log *slog.Logger) *Server {
	return &Server{
		handlers:          handlers,
		log:               log,
		readHeaderTimeout: readHeaderTimeout,
		readTimeout:       readTimeout,
		idleTimeout:       idleTimeout,
		shutdownTimeout:   shutdownTimeout,
	}
}

// ListenHTTP serves HTTP on addr until ctx is cancelled. Routes and Swagger docs are
// generated from the action registry (actions.go) — add endpoints there, not here.
func (s *Server) ListenHTTP(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	h := s.handlers

	for _, a := range registry {
		a := a
		mux.HandleFunc(a.Method+" "+a.mountPath(), func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
			env, err := a.envelope(r)
			if err == nil {
				p, authErr := s.httpPrincipal(r)
				if authErr != nil {
					writeReply(w, authErr.reply())
					return
				}
				env.principal = p
				// Before any body is written, and only with an identity: its absence on a 401 is
				// what tells a client to ask for a credential (CLAUDE.md).
				if p != nil {
					w.Header().Set(actorHeader, p.Actor())
				}
			}
			if err != nil {
				// The envelope fails only on non-JSON or an oversized body: always the
				// caller's fault.
				writeReply(w, invalid("bad request: %w", err).reply())
				return
			}
			if authErr := authorize(a, env.principal); authErr != nil {
				writeReply(w, authErr.reply())
				return
			}
			writeReply(w, a.handle(h, env))
		})
	}

	// Unauthenticated, so under publicPrefix: ingress rules are written from the prefix
	// (specs/api-auth.md §1). Nothing here derives from a user's data.
	mux.HandleFunc("GET "+publicPrefix+"/docs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, swaggerUIHTML("genroc API", publicPrefix+"/openapi.json"))
	})

	mux.HandleFunc("GET "+publicPrefix+"/openapi.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(buildSpec())
	})

	// Derived from nothing a caller stored, like the spec above. Its path differs from the
	// released genroc.org/process-schema.json on purpose (CLAUDE.md).
	mux.HandleFunc("GET "+publicPrefix+"/process-schema.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(defschema.Process())
	})

	// Gated: generated from a stored definition, it discloses the caller's data. Not a registry
	// action (it answers HTML and raw JSON), so it must call guard itself.
	mux.HandleFunc("GET /api/definitions/{name}/docs", func(w http.ResponseWriter, r *http.Request) {
		if err := s.guard(r, PermRead); err != nil {
			writeReply(w, err.reply())
			return
		}
		name := r.PathValue("name")
		specURL := "/api/definitions/" + name + "/openapi.json"
		if v := r.URL.Query().Get("version"); v != "" {
			specURL += "?version=" + v
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, swaggerUIHTML(name+" — genroc API", specURL))
	})

	mux.HandleFunc("GET /api/definitions/{name}/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		if err := s.guard(r, PermRead); err != nil {
			writeReply(w, err.reply())
			return
		}
		version := 0
		if v := r.URL.Query().Get("version"); v != "" {
			if parsed, err := strconv.Atoi(v); err == nil {
				version = parsed
			}
		}
		data, err := h.ProcessSpec(r.PathValue("name"), version)
		if err != nil {
			// Classified like every route, so a broken spec build is a 500, not an
			// unknown process.
			writeReply(w, errReply(err))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: s.readHeaderTimeout,
		ReadTimeout:       s.readTimeout,
		IdleTimeout:       s.idleTimeout,
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-ctx.Done()
		// Bounded: a request that never finishes must not hold the drain open past the
		// point the supervisor is willing to wait, or the process is SIGKILLed instead.
		shutCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
		defer cancel()
		srv.Shutdown(shutCtx)
	}()

	s.log.Info("HTTP listening", "addr", addr)
	err := srv.ListenAndServe()
	if !errors.Is(err, http.ErrServerClosed) {
		// A bind failure, and one that happens before ctx is ever cancelled — so the
		// goroutine above is still parked on ctx.Done() and waiting on it would hang.
		return err
	}
	// ListenAndServe returns before in-flight requests finish; without this wait, exit
	// severs them (CLAUDE.md).
	<-drained
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
	return s.acceptLoop(ctx, ln, false)
}

func (s *Server) ListenUDS(ctx context.Context, path string) error {
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listen uds %s: %w", path, err)
	}
	s.log.Info("UDS listening", "path", path)
	// A unix socket's file mode is the boundary, which is the standard answer for local IPC
	// and the one the docker socket uses. specs/api-auth.md §5.
	return s.acceptLoop(ctx, ln, true)
}

func (s *Server) acceptLoop(ctx context.Context, ln net.Listener, trustedTransport bool) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			// Normal shutdown. errors.Is, not message matching: a mismatch would turn a
			// clean shutdown into a logged error and a hot retry loop.
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			s.log.Error("accept error", "err", err)
			continue
		}
		go s.handleConn(conn, trustedTransport)
	}
}

func (s *Server) handleConn(conn net.Conn, trustedTransport bool) {
	defer conn.Close()
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	for {
		var env Envelope
		if err := dec.Decode(&env); err != nil {
			return
		}
		// A trusted transport is authorised by the filesystem. Otherwise the credential rides in
		// the envelope; `principal` is unexported so the wire cannot set it.
		if trustedTransport {
			env.principal = anonymousAdmin()
		} else {
			p, authErr := s.principalFor(context.Background(), env.Token)
			if authErr != nil {
				_ = enc.Encode(authErr.reply())
				return
			}
			env.principal = p
		}
		env.Token = ""
		if err := enc.Encode(s.handlers.Handle(env)); err != nil {
			s.log.Warn("write reply", "err", err)
			return
		}
	}
}

// errorBody mirrors Reply's failure half, so all three transports report the same facts
// under the same names.
type errorBody struct {
	Error  string             `json:"error"`
	Code   Code               `json:"code"`
	Fields []model.FieldError `json:"fields,omitempty"`
}

// writeReply maps Code to a status through errors.go's one table. Unclassified is 500, not
// 400, which keeps the remaining unclassified paths findable.
func writeReply(w http.ResponseWriter, r Reply) {
	w.Header().Set("Content-Type", "application/json")
	if !r.OK {
		w.WriteHeader(statusOf(r.Code))
		json.NewEncoder(w).Encode(errorBody{Error: r.Error, Code: r.Code, Fields: r.Fields})
		return
	}
	// An assertion carries its own status; a 204 must have no body, which outcomeReply
	// ensures by leaving Data empty.
	if r.Outcome != "" {
		w.WriteHeader(statusOfOutcome(r.Outcome))
		if len(r.Data) == 0 {
			return
		}
	}
	json.NewEncoder(w).Encode(r.Data)
}
