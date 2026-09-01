package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"fmt"
	"genroc/internal/api"
	"genroc/internal/db"
	"genroc/internal/engine"
	"genroc/internal/logview"
	"net"
	"strings"
)

// Set at build time: -ldflags "-X main.version=0.1.0".
var (
	version = "dev"
	commit  = ""
)

func main() {
	// Subcommands are dispatched before flag parsing, since `genroc token …` takes its own
	// flags and must work without the server's. specs/api-auth.md §5.3.
	if len(os.Args) > 1 && os.Args[1] == "token" {
		runTokenCmd(os.Args[2:])
		return
	}
	dbPath := flag.String("db", "genroc.db", "SQLite database file path")
	pgDSN := flag.String("pg", "", "PostgreSQL DSN (e.g. postgres://user:pass@host/db). When set, --db is ignored.")
	pgMaxOpenConns := flag.Int("pg-max-open-conns", 50, "PostgreSQL connection pool size, and with it the group-commit batch-width ceiling: only transactions in flight together coalesce into one WAL flush, so the pool bounds how many ever do. Size a worker fleet so workers*pg-max-open-conns stays under the server's max_connections. Ignored for SQLite.")
	pgCommitDelay := flag.Int("pg-commit-delay", 0, "Hold each PostgreSQL WAL flush back by this many microseconds so more commits coalesce into it (PostgreSQL commit_delay, set per session). Costs no durability - every commit is still flushed before it is acknowledged - and Postgres skips the delay unless commit_siblings (default 5) transactions are open, so narrow workloads are unaffected. Throughput peaks well below the flush latency and drops past it: on a 4ms F_FULLFSYNC disk 500us was the best value measured and 10000us was 26% worse than doing nothing. The gain itself is workload- and load-dependent (measured between +3% and +33% for the same 500us on the same machine), and shows up mainly as steadier throughput under contention rather than a higher ceiling. Measure on your own storage; 0 (the default) disables. Requires a superuser connection. Ignored for SQLite.")
	sqliteSync := flag.String("sqlite-synchronous", "FULL", "SQLite durability (PRAGMA synchronous): FULL (default; fsync every commit for full power-loss durability, matching Postgres synchronous_commit=on) or NORMAL (faster; durable across a process crash but may lose the last commits on power loss). Note FULL is bounded by your disk's serial fsync rate - SQLite has one writer and no group commit, so unlike PostgreSQL it cannot trade latency for batch width. Ignored for PostgreSQL.")
	sqliteFullFsync := flag.Bool("sqlite-fullfsync", false, "Use F_FULLFSYNC on macOS, where plain fsync(2) returns before the drive flushes its write cache — without this, --sqlite-synchronous=FULL is not actually power-loss durable on Apple hardware. Costs ~4ms/commit on an M1. No effect on other platforms or for PostgreSQL.")
	durability := flag.String("durability", "only-once", "How much of the write path is flushed to disk before it is acknowledged. only-once (default) flushes what cannot be replayed - work handed in from outside, and only_once tasks - and lets ordinary task progress replay after a power cut, which the at-least-once contract already allows. terminal additionally flushes process ends, so a finished process cannot rewind to running. strict flushes every commit, so no completed task ever repeats. Below strict, a client polling an instance can see a state it already passed after an unclean shutdown. See specs/durability-levels.md.")
	authMode := flag.String("auth", "none", "How callers are identified: none (default; every request is treated as an operator) or token (a genroc_sk_* credential in Authorization: Bearer, hashed in the database). A unix socket is authorised by its file mode either way. See specs/api-auth.md.")
	uiDir := flag.String("ui", "", "Serve a built frontend from this directory at `/`. Same origin as the API, which is what keeps CORS out of the picture entirely; see frontend/README.md.")
	authConfig := flag.String("auth-config", "", "Path to an auth config file (YAML) enabling `mode: header` — a proxy authenticates the caller and forwards the result, and a role map turns the asserted roles into permissions. Combines with -auth token: a deployment runs a proxy for people and tokens for machines. See specs/api-auth.md §2, §4.")
	seedTokens := flag.String("seed-tokens", "", "Credentials the operator generated, as a comma-separated list of `label=perms=secret` (perms itself is +-separated), e.g. \"admin=admin=genroc_sk_...,evaluator=worker=genroc_sk_...\" ($GENROC_SEED_TOKENS). Each is stored if absent and ignored if present, so a restart is a no-op and rotation is additive. The secret never originates here, so it never reaches these logs.")
	bootstrapToken := flag.String("bootstrap-token", "", "In token mode, the admin credential to create when the deployment has no live one ($GENROC_BOOTSTRAP_TOKEN). Idempotent: ignored once an admin token exists, so it doubles as declarative recovery. Omit it and one is generated and printed once, to stderr.")
	httpAddr := flag.String("http", ":8448", "HTTP listen address (empty to disable)")
	tcpAddr := flag.String("tcp", "", "TCP listen address, e.g. 127.0.0.1:9090 (empty to disable)")
	udsPath := flag.String("uds", "", "Unix socket path, e.g. /tmp/genroc.sock (empty to disable)")
	pollMs := flag.Int("poll", 500, "Engine poll interval in milliseconds")
	maxConcurrent := flag.Int("max-concurrent", 200, "Max instances advanced concurrently. Too high overwhelms the DB/lease renewer and in-flight work starts losing its leases (lease_lost audit entries); raise --lease-duration or the DB connection pool before raising this much further.")
	immediateRetries := flag.Bool("immediate-retries", false, "Disable retry backoff (retries fire instantly); for testing only")
	leaseDuration := flag.Duration("lease-duration", 10*time.Second, "How long a claimed instance is leased to a worker before another worker may reclaim it on crash")
	leaseRenewInterval := flag.Duration("lease-renew-interval", 3*time.Second, "How often a worker re-stamps its leases; must be comfortably shorter than --lease-duration")
	pprofAddr := flag.String("pprof", "", "pprof listen address, e.g. localhost:6060 (empty to disable)")
	logLevel := flag.String("log", "info", "Log level: debug, info, warn, error")
	logMode := flag.String("log-mode", "basic", "Console output: basic (no data body), detail (+ data body), or json (one JSON object per line); same modes as genctl logs. Times are UTC, so a fleet's logs collate; genctl renders the reader's local zone")
	logPayloads := flag.Bool("log-payloads", true, "Capture truncated request/response snippets in per-instance audit logs")
	logPayloadBytes := flag.Int("log-payload-bytes", 2048, "Max bytes per captured request/response snippet in audit logs")
	logRetention := flag.Duration("log-retention", 168*time.Hour, "Delete per-instance audit logs older than this; 0 = keep forever")
	objectGrace := flag.Duration("object-grace", time.Hour, "How long a released large value stays fetchable by a reference already handed out. A read hands out references and fetching them is a second call, so the data can move on in between; this is the window in which that cannot lose. Raise it for slow consumers, lower it for processes that churn big values in a loop.")
	showVersion := flag.Bool("version", false, "Print the version and exit.")
	flag.Parse()
	if *showVersion {
		fmt.Println(versionString())
		return
	}

	mode, err := logview.ParseMode(*logMode)
	if err != nil {
		newLogger("error", logview.ModeBasic).Error("invalid --log-mode", "err", err)
		os.Exit(1)
	}
	log := newLogger(*logLevel, mode)

	if *leaseRenewInterval >= *leaseDuration {
		log.Error("--lease-renew-interval must be shorter than --lease-duration",
			"lease_renew_interval", *leaseRenewInterval, "lease_duration", *leaseDuration)
		os.Exit(1)
	}

	// Validated before the database is opened: a bad flag should not reach the disk.
	durLevel, err := db.ParseDurability(*durability)
	if err != nil {
		log.Error("invalid --durability", "error", err)
		os.Exit(1)
	}

	var database *db.DB
	var dbErr error
	if *pgDSN != "" {
		var pgOpts []db.PostgresOption
		if *pgCommitDelay > 0 {
			pgOpts = append(pgOpts, db.WithCommitDelay(*pgCommitDelay))
		}
		database, dbErr = db.OpenPostgres(*pgDSN, *pgMaxOpenConns, pgOpts...)
	} else {
		var sqliteOpts []db.SQLiteOption
		if *sqliteFullFsync {
			sqliteOpts = append(sqliteOpts, db.WithFullFsync())
		}
		database, dbErr = db.OpenSQLite(*dbPath, *sqliteSync, sqliteOpts...)
	}
	if dbErr != nil {
		log.Error("open database", "err", dbErr)
		os.Exit(1)
	}
	defer database.Close()
	database.SetDurability(durLevel)
	if *pgDSN != "" {
		log.Info("database opened", "driver", "postgres", "durability", durLevel.String(),
			"commit_delay_us", *pgCommitDelay)
	} else {
		log.Info("database opened", "driver", "sqlite", "path", *dbPath,
			"synchronous", *sqliteSync, "durability", durLevel.String())
	}

	logCfg := engine.LogConfig{
		Payloads:     *logPayloads,
		PayloadBytes: *logPayloadBytes,
		Retention:    *logRetention,
		Mode:         mode,
	}
	database.SetObjectGrace(*objectGrace)
	eng := engine.New(database, time.Duration(*pollMs)*time.Millisecond, *maxConcurrent, *immediateRetries, *leaseDuration, *leaseRenewInterval, logCfg, log)
	handlers := api.NewHandlers(database, eng)
	srv := api.NewServer(handlers, log)

	if *uiDir != "" {
		srv.SetUI(*uiDir)
		log.Info("serving UI", "dir", *uiDir)
	}

	// A forwarded identity and a bearer token are independent: either may be configured, and a
	// deployment serving both people and machines configures both.
	if *authConfig != "" {
		cfg, err := api.LoadAuthConfig(*authConfig)
		if err != nil {
			log.Error("auth-config", "err", err)
			os.Exit(1)
		}
		if cfg.Mode == "header" {
			h, err := api.NewHeaderAuth(cfg)
			if err != nil {
				log.Error("auth-config", "err", err)
				os.Exit(1)
			}
			srv.SetHeaderAuth(h)
			log.Info("API authentication enabled", "mode", "header",
				"subject_header", cfg.Header.Subject, "trusted_proxies", cfg.Header.TrustedProxies)
		}
	}

	switch *authMode {
	case "none":
		// The pre-auth default. Loud rather than silent when it is also reachable off-host:
		// `docker run -p` puts an unauthenticated PUT /definitions on the network, and that
		// should be a decision. specs/api-auth.md §6.
		if exposedAddr(*httpAddr) && *authConfig == "" {
			log.Warn("API is UNAUTHENTICATED and bound beyond loopback — anyone who reaches this port can register a definition, which is arbitrary code execution on this server. Use -auth token, or bind to localhost.",
				"addr", *httpAddr)
		}
	case "token":
		seeds := *seedTokens
		if seeds == "" {
			seeds = os.Getenv("GENROC_SEED_TOKENS")
		}
		if seeds != "" {
			n, skipped, err := seedSuppliedTokens(database, seeds)
			if err != nil {
				log.Error("seed-tokens", "err", err)
				os.Exit(1)
			}
			if len(skipped) > 0 {
				// Named, so a credential that vanished by accident looks different from one
				// removed on purpose.
				log.Info("seeded operator-supplied credentials", "created", n,
					"no_secret_supplied", strings.Join(skipped, ","))
			} else {
				log.Info("seeded operator-supplied credentials", "created", n)
			}
		}
		secret := *bootstrapToken
		if secret == "" {
			secret = os.Getenv("GENROC_BOOTSTRAP_TOKEN")
		}
		// A deployment with header mode already has a way in — the proxy identifies an operator
		// and the role map gives them admin — so minting a bootstrap credential nobody asked
		// for, and printing it to a log, is pure exposure. Skip it and let the proxy be the
		// answer; `genroc token create` remains the break-glass path either way.
		if *authConfig != "" && secret == "" {
			log.Info("skipping bootstrap token", "reason", "header mode provides an operator path")
			srv.SetAuthenticator(api.NewTokenAuth(database))
			log.Info("API authentication enabled", "mode", "token")
			break
		}
		tok, created, err := database.EnsureBootstrapToken(context.Background(), "bootstrap", secret)
		if err != nil {
			log.Error("bootstrap token", "err", err)
			os.Exit(1)
		}
		if created && *bootstrapToken == "" && os.Getenv("GENROC_BOOTSTRAP_TOKEN") == "" {
			// Printed once, and nowhere else: this is the weakest of §5.3's three paths
			// because log aggregation ships it off the box.
			fmt.Fprintf(os.Stderr, "genroc: created bootstrap admin token %s\n  %s\n"+
				"  This is the only time it is shown, and it is now in your logs — rotate it.\n", tok.ID, tok.Secret)
		}
		srv.SetAuthenticator(api.NewTokenAuth(database))
		log.Info("API authentication enabled", "mode", "token")
	default:
		log.Error("unknown -auth mode", "mode", *authMode, "valid", "none, token")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	var fatalErr error
	// fatal records the first fatal error from any subsystem and winds the process
	// down (cancelling ctx shuts down every listener and the engine). Reads happen
	// after wg.Wait(), which is ordered after these writes.
	fatal := func(what string, err error) {
		if fatalErr == nil {
			fatalErr = err
		}
		log.Error(what+"; shutting down", "err", err)
		stop()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		// Run drains in-flight work before returning; lease pressure repairs or is
		// refused per-write (lease_lost), never an exit. specs/lease-fencing.md.
		eng.Run(ctx)
	}()

	if *pprofAddr != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Info("pprof listening", "addr", *pprofAddr)
			if err := http.ListenAndServe(*pprofAddr, nil); err != nil {
				log.Error("pprof server", "err", err)
			}
		}()
	}

	if *httpAddr != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A listener that cannot bind (e.g. the port is still held) returns a
			// non-nil error here — graceful shutdown returns nil. A server that can't
			// serve its API is useless, so treat it as fatal instead of running on
			// headless; the supervisor then restarts it (or a test fails loudly).
			if err := srv.ListenHTTP(ctx, *httpAddr); err != nil {
				fatal("HTTP server failed", err)
			}
		}()
	}

	if *tcpAddr != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.ListenTCP(ctx, *tcpAddr); err != nil {
				fatal("TCP server failed", err)
			}
		}()
	}

	if *udsPath != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.ListenUDS(ctx, *udsPath); err != nil {
				fatal("UDS server failed", err)
			}
		}()
	}

	<-ctx.Done()
	log.Info("shutting down")
	wg.Wait()
	if fatalErr != nil {
		os.Exit(1)
	}
}

// newLogger builds the server console logger on the shared logview handler, so its rows
// match genctl logs. level is the severity threshold; mode picks the layout.
func newLogger(level string, mode logview.Mode) *slog.Logger {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(logview.NewHandler(os.Stderr, l, mode))
}

// exposedAddr reports whether a listen address can be reached from off-host. An empty host
// (":8448") binds every interface, which is what a container publishes.
func exposedAddr(addr string) bool {
	if addr == "" {
		return false
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return true // unparseable: assume the risky reading
	}
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return false
	}
	return true
}

// seedSuppliedTokens stores credentials the operator generated. The format is deliberately
// flat — `label=perms=secret`, perms joined by `+` — because it has to survive a compose
// `environment:` value and a shell, where anything richer needs quoting nobody gets right.
// Token bodies are base64url without padding, so they carry no `=` of their own.
func seedSuppliedTokens(database *db.DB, spec string) (created int, skipped []string, err error) {
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, "=", 3)
		if len(parts) != 3 {
			return 0, nil, fmt.Errorf("bad entry %q: want label=perms=secret", redactSeed(entry))
		}
		label, permSpec, secret := parts[0], parts[1], parts[2]
		// An entry whose SECRET is empty is one the operator removed after first start —
		// which is the intended lifecycle for an admin credential they have since stored
		// elsewhere. Seeding is idempotent, so the row is already there; skipping is the
		// no-op that keeps a restart working. A malformed entry (fewer than three parts) is
		// still an error, so this cannot swallow a typo.
		if secret == "" {
			skipped = append(skipped, label)
			continue
		}
		var perms []string
		for _, p := range strings.Split(permSpec, "+") {
			if p = strings.TrimSpace(p); p != "" {
				perms = append(perms, p)
			}
		}
		if label == "" || len(perms) == 0 {
			return 0, nil, fmt.Errorf("bad entry %q: label and perms are both required", redactSeed(entry))
		}
		ok, seedErr := database.SeedToken(context.Background(), label, perms, secret)
		if seedErr != nil {
			return 0, nil, seedErr
		}
		if ok {
			created++
		}
	}
	return created, skipped, nil
}

// redactSeed keeps a malformed entry out of the logs intact — the reason it is malformed is
// usually a mangled separator, and the rest of the line is still a credential.
func redactSeed(entry string) string {
	if i := strings.Index(entry, db.TokenPrefix); i >= 0 {
		return entry[:i+len(db.TokenPrefix)] + "..."
	}
	return entry
}
