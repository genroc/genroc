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

	"genroc/internal/api"
	"genroc/internal/db"
	"genroc/internal/engine"
	"genroc/internal/logview"
)

func main() {
	dbPath := flag.String("db", "genroc.db", "SQLite database file path")
	pgDSN := flag.String("pg", "", "PostgreSQL DSN (e.g. postgres://user:pass@host/db). When set, --db is ignored.")
	pgMaxOpenConns := flag.Int("pg-max-open-conns", 50, "PostgreSQL connection pool size. Size a worker fleet so workers*pg-max-open-conns stays under the server's max_connections. Ignored for SQLite.")
	sqliteSync := flag.String("sqlite-synchronous", "FULL", "SQLite durability (PRAGMA synchronous): FULL (default; fsync every commit for full power-loss durability, matching Postgres synchronous_commit=on) or NORMAL (faster; durable across a process crash but may lose the last commits on power loss). Ignored for PostgreSQL.")
	sqliteFullFsync := flag.Bool("sqlite-fullfsync", false, "Use F_FULLFSYNC on macOS, where plain fsync(2) returns before the drive flushes its write cache — without this, --sqlite-synchronous=FULL is not actually power-loss durable on Apple hardware. Costs ~4ms/commit on an M1. No effect on other platforms or for PostgreSQL.")
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
	flag.Parse()

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

	var database *db.DB
	var dbErr error
	if *pgDSN != "" {
		database, dbErr = db.OpenPostgres(*pgDSN, *pgMaxOpenConns)
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
	if *pgDSN != "" {
		log.Info("database opened", "driver", "postgres")
	} else {
		log.Info("database opened", "driver", "sqlite", "path", *dbPath, "synchronous", *sqliteSync)
	}

	logCfg := engine.LogConfig{
		Payloads:     *logPayloads,
		PayloadBytes: *logPayloadBytes,
		Retention:    *logRetention,
		Mode:         mode,
	}
	eng := engine.New(database, time.Duration(*pollMs)*time.Millisecond, *maxConcurrent, *immediateRetries, *leaseDuration, *leaseRenewInterval, logCfg, log)
	handlers := api.NewHandlers(database, eng)
	srv := api.NewServer(handlers, log)

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
