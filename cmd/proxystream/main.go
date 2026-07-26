package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/aluzed/freebox-proxystream/internal/api"
	"github.com/aluzed/freebox-proxystream/internal/cache"
	"github.com/aluzed/freebox-proxystream/internal/config"
	"github.com/aluzed/freebox-proxystream/internal/db"
	"github.com/aluzed/freebox-proxystream/internal/ftpclient"
	"github.com/aluzed/freebox-proxystream/internal/mediasource"
	"github.com/aluzed/freebox-proxystream/internal/rangeproxy"
	"github.com/aluzed/freebox-proxystream/internal/scanner"
	"github.com/aluzed/freebox-proxystream/internal/subs"
	"github.com/aluzed/freebox-proxystream/internal/thumbnail"
	"github.com/aluzed/freebox-proxystream/internal/transcode"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "users":
			exitOn(cmdUsers())
			return
		case "passwd":
			exitOn(cmdPasswd(os.Args[2:]))
			return
		case "useradd":
			exitOn(cmdUserAdd(os.Args[2:]))
			return
		case "hashpass":
			hashpass(os.Args[2:])
			return
		case "version":
			fmt.Println("proxystream", version())
			return
		case "help", "-h", "--help":
			usage()
			return
		}
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`proxystream - streaming proxy for media on a Freebox FTP share

Usage:
  proxystream                    start the server (reads .env from the working directory)
  proxystream users              list accounts
  proxystream passwd [user]      change an account password
  proxystream useradd <user>     create an account
  proxystream version            print the version

Accounts live in the SQLite database, never in .env. The first one is normally
created from the web UI on first start; useradd is there for headless setups.
`)
}

func version() string {
	return "0.1.0"
}

func exitOn(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func hashpass(args []string) {
	plain, err := readPassword(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(plain) < 8 {
		fmt.Fprintln(os.Stderr, "password must be at least 8 characters")
		os.Exit(1)
	}
	hash, err := db.HashPassword(plain)
	if err != nil {
		fmt.Fprintln(os.Stderr, "could not hash password:", err)
		os.Exit(1)
	}
	fmt.Println(hash)
}

// readPassword takes the password from the command line, from an interactive
// prompt with the echo turned off, or from a pipe. It reads whole lines: a
// passphrase with spaces in it must not be silently truncated to its first word.
func readPassword(args []string) (string, error) {
	if len(args) > 0 {
		return args[0], nil
	}

	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, "Password: ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", fmt.Errorf("could not read password: %w", err)
		}
		typed := string(b)

		fmt.Fprint(os.Stderr, "Confirm:  ")
		b2, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", fmt.Errorf("could not read password: %w", err)
		}
		if typed != string(b2) {
			return "", errors.New("the two passwords do not match")
		}
		return typed, nil
	}

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("could not read password from stdin: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func run() error {
	cfg, err := config.Load(".env")
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel(),
	}))
	slog.SetDefault(logger)

	for _, dir := range []string{cfg.CacheDir, cfg.SubtitleDir, cfg.ThumbDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}

	sdb, err := db.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer sdb.Close()
	store := db.NewStore(sdb)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := store.PurgeExpiredSessions(ctx); err != nil {
		logger.Warn("could not purge expired sessions", "err", err)
	}

	ftp := ftpclient.New(ftpclient.Config{
		Host:    cfg.FTPHost,
		User:    cfg.FTPUser,
		Pass:    cfg.FTPPass,
		Timeout: cfg.FTPTimeout,
	}, logger)
	defer ftp.Close()

	// A bad FTP config is worth a loud warning, but not a refusal to start:
	// the UI is still useful for fixing it.
	pingCtx, cancelPing := context.WithTimeout(ctx, 15*time.Second)
	if err := ftp.Ping(pingCtx); err != nil {
		logger.Warn("FTP is not reachable yet", "host", cfg.FTPHost, "err", err)
	} else {
		logger.Info("FTP connection established", "host", cfg.FTPHost)
	}
	cancelPing()

	// A misconfigured encoder must fail here, not halfway through a film.
	encoder, err := transcode.LookupEncoder(cfg.VideoEncoder)
	if err != nil {
		return err
	}
	if err := transcode.VerifyEncoder(ctx, cfg.FFmpegBin, encoder); err != nil {
		return err
	}
	logger.Info("video encoder", "name", encoder.Name, "hardware", encoder.Hardware)

	mgr := transcode.NewManager(cfg, store, logger)
	janitor := cache.NewJanitor(cfg, store, mgr, logger)
	if err := janitor.Reconcile(ctx); err != nil {
		logger.Warn("cache reconciliation failed", "err", err)
	}

	thumbs := thumbnail.New(cfg)
	sc := scanner.New(cfg, store, ftp, thumbs, logger)

	srv, err := api.New(api.Deps{
		Config:  cfg,
		Store:   store,
		FTP:     ftp,
		Manager: mgr,
		Scanner: sc,
		Janitor: janitor,
		Subs:    subs.New(cfg),
		Thumbs:  thumbs,
		Log:     logger,
	})
	if err != nil {
		return fmt.Errorf("build server: %w", err)
	}

	internal := &http.Server{
		Addr:    cfg.InternalAddr,
		Handler: rangeproxy.New(mediasource.New(store, ftp), logger).Handler(),
		// ffmpeg holds a range request open for as long as it reads, so no
		// write timeout here.
		ReadHeaderTimeout: 15 * time.Second,
	}
	public := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); mgr.Run(ctx) }()
	go func() { defer wg.Done(); janitor.Run(ctx) }()
	go func() { defer wg.Done(); sc.Run(ctx) }()

	errCh := make(chan error, 2)
	go func() {
		logger.Info("range proxy listening (loopback only)", "addr", cfg.InternalAddr)
		if err := internal.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("range proxy: %w", err)
		}
	}()
	go func() {
		logger.Info("server listening", "addr", cfg.HTTPAddr, "default_quality", cfg.DefaultHeight)
		if err := public.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http server: %w", err)
		}
	}()

	select {
	case err := <-errCh:
		stop()
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	public.Shutdown(shutdownCtx)
	internal.Shutdown(shutdownCtx)
	mgr.Shutdown("shutdown", 10*time.Second)
	wg.Wait()

	logger.Info("stopped")
	return nil
}

func logLevel() slog.Level {
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
