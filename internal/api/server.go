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

	"genroc/internal/model"
	"path/filepath"
)

// HTTP listener limits. Deliberately no WriteTimeout: /tick blocks until its claimed
// instances finish, so any useful ceiling would sever legitimate long ticks.
// readHeaderTimeout is what bounds a connection that opens and sends nothing.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 60 * time.Second
	idleTimeout       = 120 * time.Second
	shutdownTimeout   = 15 * time.Second
	// maxRequestBytes caps a request body across every route. Comfortably above the
	// largest definition batch anyone submits, and below the point where a body costs
	// more to buffer than the process it describes.
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

	// uiDir serves a built single-page app at `/`. Empty means no UI, and `/` 404s.
	uiDir string

	// header is `mode: header`, checked BEFORE auth: a request arriving through the trusted
	// proxy is already identified, and asking the token store about a credential it does not
	// carry would only cost a query. Both may be configured at once — a deployment runs an SSO
	// proxy for people and tokens for machines, and each request is admitted by whichever
	// recognises it. specs/api-auth.md §2.
	header *HeaderAuth
}

// SetUI serves a built frontend at `/`. Called once at startup, before Listen*.
func (s *Server) SetUI(dir string) { s.uiDir = dir }

// SetHeaderAuth turns on `mode: header`. Called once at startup, before Listen*.
func (s *Server) SetHeaderAuth(h *HeaderAuth) { s.header = h }

// guard authorizes a hand-written route, for the handful that cannot be registry actions
// because they answer with something other than a Reply. Everything else goes through
// authorize in the registry loop; this is the same decision spelled at the call site.
func (s *Server) guard(r *http.Request, allow ...Perm) *Error {
	p, err := s.httpPrincipal(r)
	if err != nil {
		return err
	}
	return authorize(actionDef{Name: r.URL.Path, Allow: allow}, p)
}

// httpPrincipal resolves who is asking over HTTP, which is the only transport carrying headers.
// A forwarded identity is preferred over a bearer token because a browser behind the proxy has
// no token to send; a machine bypasses the proxy and has no forwarded identity. Neither can
// shadow the other.
func (s *Server) httpPrincipal(r *http.Request) (*Principal, *Error) {
	if s.header != nil {
		if p := s.header.PrincipalFrom(r); p != nil {
			return p, nil
		}
	}
	return s.principalFor(r.Context(), bearerToken(r.Header.Get("Authorization")))
}

// SetAuthenticator turns on an identity mode. Called once at startup, before Listen*.
func (s *Server) SetAuthenticator(a Authenticator) { s.auth = a }

// principalFor resolves the caller. An authenticator that cannot DECIDE (its database is
// unreachable) fails the request rather than answering "unauthenticated": a valid credential
// refused as invalid is a lie the operator never sees, and 503 is the honest answer.
func (s *Server) principalFor(ctx context.Context, credential string) (*Principal, *Error) {
	if s.auth == nil && s.header == nil {
		return anonymousAdmin(), nil
	}
	if s.auth == nil {
		// header mode alone: a caller the proxy did not identify has no other way in.
		return nil, nil
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
			}
			if err != nil {
				// The envelope only fails on a body that is not JSON at all, or one
				// past maxRequestBytes, so this is always the caller's fault, never
				// the server's.
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

	// The UI, at the root. Serving it from genroc is what keeps the browser on ONE origin, so
	// `/api` is same-origin and no CORS exists anywhere in the system (§5.1). Unauthenticated
	// on purpose: it is a static bundle that discloses nothing — what it can DO is decided by
	// the credential it obtains, and a deployment behind an SSO proxy puts the login in front
	// of this route anyway.
	if s.uiDir != "" {
		files := http.FileServer(http.Dir(s.uiDir))
		mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
			// A single-page app owns its own routing, so an unknown path serves index.html
			// rather than 404 — otherwise a deep link only works when typed at the root.
			if _, err := os.Stat(filepath.Join(s.uiDir, filepath.Clean(r.URL.Path))); err != nil {
				http.ServeFile(w, r, filepath.Join(s.uiDir, "index.html"))
				return
			}
			files.ServeHTTP(w, r)
		})
	}

	// The session exchange: what turns a browser's proxy session into a bearer token.
	//
	// It lives OUTSIDE /api on purpose. A deployment routes /api/* around the SSO proxy so
	// machine callers never meet a login redirect (§5.1) — so a route that needs the proxy's
	// injected identity cannot be under it. The browser zone `/*` already goes through the
	// proxy, and this falls under that rule with no new ingress config.
	//
	// **It must never permit a cross-origin READ.** It is authenticated by whatever ambient
	// credential the browser holds (the proxy's cookie), so a malicious page can cause the
	// request — what stops the token escaping is that the page cannot see the response. Adding
	// `Access-Control-Allow-Origin` here hands every site on the internet a token.
	mux.HandleFunc("GET /session/token", func(w http.ResponseWriter, r *http.Request) {
		if s.header == nil {
			writeReply(w, unsupported("session exchange needs `mode: header`; this server has none").reply())
			return
		}
		// A minted token is only useful if something can verify it on the next request. Header
		// mode alone cannot: it identifies a caller by a forwarded header, and a bearer token
		// means nothing to it. Refusing here beats handing the browser a credential that 401s
		// on every call it makes with it — which is what this did first.
		if s.auth == nil {
			writeReply(w, unsupported("session exchange mints a bearer token, so it needs "+
				"`-auth token` as well as header mode — otherwise nothing can verify what it issues").reply())
			return
		}
		p := s.header.PrincipalFrom(r)
		if p == nil {
			writeReply(w, apiErrf(CodeUnauthenticated,
				"no identity was forwarded — this route must be reached THROUGH the proxy").reply())
			return
		}
		tok, err := s.handlers.mintSessionToken(r.Context(), p, s.header.SessionTTL())
		if err != nil {
			writeReply(w, errReply(err))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(map[string]any{"token": tok.Secret, "subject": p.Subject})
	})

	// The generic API documentation is UNAUTHENTICATED, and lives under its own prefix so that
	// is legible from a routing rule. Under /api/ it read as gated and was not — the exact
	// mismatch specs/api-auth.md §1 exists to prevent, since a deployment writes its ingress
	// rules from the prefix. Nothing here is derived from a user's data: it is the same spec
	// published at genroc.org, and the UI is a static page.
	mux.HandleFunc("GET "+publicPrefix+"/docs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, swaggerUIHTML("genroc API", publicPrefix+"/openapi.json"))
	})

	mux.HandleFunc("GET "+publicPrefix+"/openapi.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(buildSpec())
	})

	// A schema document, unauthenticated and derived from nothing a caller stored — the same
	// class as the OpenAPI spec above, so it lives beside it. The docs site publishes the same
	// bytes at genroc.org/process-schema.json; that is a released artifact at a stable public
	// URL, while this is the convenience for an unreleased build, and getting-started.mdx says
	// which to use. They do not have to share a path.
	mux.HandleFunc("GET "+publicPrefix+"/process-schema.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(buildProcessDefinitionSchema())
	})

	// The PER-PROCESS documentation stays under /api/ and is gated: it is generated from a
	// stored definition, so it discloses process names, input schemas and task structure —
	// the caller's data, not ours. It cannot be a registry action because it answers with
	// HTML and raw JSON rather than a Reply, so the check is explicit here instead.
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
			// Was an unconditional 404 with a text/plain body; now it classifies like
			// every other route, so a broken spec build reports 500 rather than
			// masquerading as an unknown process.
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
	// ListenAndServe returns the moment Shutdown closes the listener, so without this the
	// caller's WaitGroup completes while requests are still in flight and process exit
	// severs them — a graceful shutdown that is graceful in name only.
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
		// A trusted transport is authorised by the filesystem, so it skips the mode entirely.
		// Everything else presents its credential in the envelope, which is the only metadata
		// channel this protocol has — `principal` is unexported precisely so the wire cannot
		// set it directly.
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
	// A successful assertion carries its own status (statusOfOutcome); every other
	// success is the implicit 200. 204 must not be given a body, and outcomeReply
	// leaves Data empty for exactly that outcome.
	if r.Outcome != "" {
		w.WriteHeader(statusOfOutcome(r.Outcome))
		if len(r.Data) == 0 {
			return
		}
	}
	json.NewEncoder(w).Encode(r.Data)
}
