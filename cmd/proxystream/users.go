package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aluzed/freebox-proxystream/internal/config"
	"github.com/aluzed/freebox-proxystream/internal/db"
)

// withStore opens the same database the server uses. These commands are meant
// to be run while the server is stopped, or at least while nobody is logged in;
// SQLite in WAL mode tolerates a concurrent reader either way.
func withStore(fn func(context.Context, *db.Store) error) error {
	cfg, err := config.Load(".env")
	if err != nil {
		return err
	}
	sdb, err := db.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", cfg.DBPath, err)
	}
	defer sdb.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return fn(ctx, db.NewStore(sdb))
}

func cmdUsers() error {
	return withStore(func(ctx context.Context, store *db.Store) error {
		users, err := store.ListUsers(ctx)
		if err != nil {
			return err
		}
		if len(users) == 0 {
			fmt.Println("No account yet. Open the web UI to create one, or run `proxystream useradd <username>`.")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tUSERNAME\tCREATED\tPASSWORD CHANGED\tACTIVE SESSIONS")
		for _, u := range users {
			sessions, err := store.SessionCount(ctx, u.ID)
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%d\n",
				u.ID, u.Username,
				u.CreatedAt.Local().Format("2006-01-02 15:04"),
				u.UpdatedAt.Local().Format("2006-01-02 15:04"),
				sessions)
		}
		return w.Flush()
	})
}

func cmdPasswd(args []string) error {
	return withStore(func(ctx context.Context, store *db.Store) error {
		users, err := store.ListUsers(ctx)
		if err != nil {
			return err
		}
		if len(users) == 0 {
			return errors.New("no account exists yet, create one with `proxystream useradd <username>`")
		}

		// args[0] is the optional username, args[1] the optional password.
		var username string
		var rest []string
		if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
			username = strings.TrimSpace(args[0])
			rest = args[1:]
		}
		if username == "" {
			if len(users) != 1 {
				return errors.New("several accounts exist, name one: `proxystream passwd <username>`")
			}
			// Single user is the normal case; do not make them type it.
			username = users[0].Username
			fmt.Fprintf(os.Stderr, "Changing the password for %q.\n", username)
		}

		password, err := readPassword(rest)
		if err != nil {
			return err
		}
		if len(password) < 8 {
			return errors.New("password must be at least 8 characters")
		}

		if err := store.SetPassword(ctx, username, password); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return fmt.Errorf("no such user %q", username)
			}
			return err
		}
		fmt.Printf("Password updated for %s. All sessions were signed out.\n", username)
		return nil
	})
}

func cmdUserAdd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: proxystream useradd <username>")
	}
	username := args[0]

	return withStore(func(ctx context.Context, store *db.Store) error {
		password, err := readPassword(args[1:])
		if err != nil {
			return err
		}
		if len(password) < 8 {
			return errors.New("password must be at least 8 characters")
		}
		hash, err := db.HashPassword(password)
		if err != nil {
			return err
		}
		if _, err := store.CreateUser(ctx, username, hash); err != nil {
			return fmt.Errorf("could not create %q (does it already exist?): %w", username, err)
		}
		fmt.Printf("Created %s.\n", username)
		return nil
	})
}
