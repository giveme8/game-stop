package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/giveme8/game-stop/server/internal/library"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(args []string) error {
	db, err := library.OpenMySQL(os.Getenv("MYSQL_DSN"))
	if err != nil {
		return err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return errors.New("database unavailable")
	}
	defer sqlDB.Close()
	if err = library.Migrate(db); err != nil {
		return errors.New("database migration failed")
	}
	output := json.NewEncoder(os.Stdout)
	if len(args) > 0 && args[0] != "serve" {
		if len(args) < 2 {
			return errors.New("usage: server [serve | users create --name NAME [--password PW] [--status active|pending] | users approve --name NAME | users reject --name NAME | users pending | tokens create --user NAME --ttl 720h | tokens revoke --id ID]")
		}
		fs := flag.NewFlagSet("admin", flag.ContinueOnError)
		name := fs.String("name", "", "account name")
		user := fs.String("user", "", "account name")
		ttl := fs.Duration("ttl", 720*time.Hour, "token lifetime (max 8760h)")
		id := fs.String("id", "", "token ID")
		password := fs.String("password", "", "account password (omit for a token-only account)")
		status := fs.String("status", "", "initial review status (active or pending)")
		if err = fs.Parse(args[2:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("unexpected arguments")
		}
		switch args[0] + " " + args[1] {
		case "users create":
			initial := library.StatusActive
			if *status != "" {
				initial = *status
			}
			u, e := library.CreateUserWithPassword(db, *name, *password, initial)
			if e != nil {
				return e
			}
			return output.Encode(u)
		case "users approve":
			u, e := library.SetUserStatus(db, *name, library.StatusActive)
			if e != nil {
				return e
			}
			return output.Encode(u)
		case "users reject":
			u, e := library.SetUserStatus(db, *name, library.StatusRejected)
			if e != nil {
				return e
			}
			return output.Encode(u)
		case "users pending":
			users, e := library.PendingUsers(db)
			if e != nil {
				return e
			}
			return output.Encode(users)
		case "tokens create":
			t, raw, e := library.CreateToken(db, *user, *ttl)
			if e != nil {
				return e
			}
			return output.Encode(map[string]any{"id": t.ID, "token": raw, "expiresAt": t.ExpiresAt})
		case "tokens revoke":
			if err = library.RevokeToken(db, *id); err != nil {
				return err
			}
			return output.Encode(map[string]bool{"revoked": true})
		default:
			return errors.New("unknown admin command")
		}
	}
	if len(args) > 1 {
		return errors.New("unexpected serve arguments")
	}
	app, err := library.NewServer(db, env("DATA_DIR", "./data"), env("PUBLIC_BASE_URL", "https://game-stop.baobaobao.online"))
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: env("LISTEN_ADDR", "127.0.0.1:8080"), Handler: app.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 15 * time.Minute, WriteTimeout: 15 * time.Minute, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()
	select {
	case err = <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return errors.New("HTTP listener failed")
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}
