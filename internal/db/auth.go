package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"golang.org/x/crypto/bcrypt"
)

var (
	ErrBadCredentials = errors.New("invalid username or password")
	ErrNoAccount      = errors.New("no account configured")
)

type User struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func HashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	return string(b), err
}

func (s *Store) HasUser(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n > 0, err
}

// CreateUser stores an account from an already-hashed password.
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string) (*User, error) {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		username, passwordHash, now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &User{ID: id, Username: username, CreatedAt: now, UpdatedAt: now}, nil
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, username, created_at, updated_at FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []User{}
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetPassword resets a password without knowing the old one. This is the
// operator escape hatch, reachable only from the CLI, which implies filesystem
// access to the database; the web UI always goes through ChangePassword.
func (s *Store) SetPassword(ctx context.Context, username, plain string) error {
	hash, err := HashPassword(plain)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, updated_at = ? WHERE username = ?`,
		hash, time.Now().UTC(), username)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	// Any device still holding a session must be logged out.
	_, err = s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE user_id IN (SELECT id FROM users WHERE username = ?)`, username)
	return err
}

func (s *Store) SessionCount(ctx context.Context, userID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE user_id = ? AND expires_at > ?`,
		userID, time.Now().UTC()).Scan(&n)
	return n, err
}

func (s *Store) GetUser(ctx context.Context, id int64) (*User, error) {
	var u User
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, created_at, updated_at FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Username, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &u, err
}

// Authenticate verifies a username/password pair. It always runs a bcrypt
// comparison, even for an unknown user, so a wrong username and a wrong
// password take the same time.
func (s *Store) Authenticate(ctx context.Context, username, password string) (*User, error) {
	var u User
	var hash string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, created_at, updated_at FROM users WHERE username = ?`, username).
		Scan(&u.ID, &u.Username, &hash, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		// Cost-matched dummy comparison against a fixed valid bcrypt hash.
		bcrypt.CompareHashAndPassword([]byte(dummyHash), []byte(password))
		return nil, ErrBadCredentials
	}
	if err != nil {
		return nil, err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return nil, ErrBadCredentials
	}
	return &u, nil
}

// bcrypt hash of a random string, used only to equalise timing.
const dummyHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"

func (s *Store) ChangePassword(ctx context.Context, userID int64, current, next string) error {
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, userID).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(current)); err != nil {
		return ErrBadCredentials
	}
	newHash, err := HashPassword(next)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`,
		newHash, time.Now().UTC(), userID); err != nil {
		return err
	}
	// A password change logs every device out.
	_, err = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

// ---------- sessions ----------

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewSession returns the opaque cookie value; only its hash is persisted.
func (s *Store) NewSession(ctx context.Context, userID int64, ttl time.Duration) (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := time.Now().UTC()
	expires := now.Add(ttl)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		hashToken(token), userID, now, expires)
	if err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

func (s *Store) SessionUser(ctx context.Context, token string) (*User, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	var u User
	var expires time.Time
	err := s.db.QueryRowContext(ctx,
		`SELECT u.id, u.username, u.created_at, u.updated_at, s.expires_at
		 FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.token_hash = ?`,
		hashToken(token)).Scan(&u.ID, &u.Username, &u.CreatedAt, &u.UpdatedAt, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if time.Now().UTC().After(expires) {
		s.DeleteSession(ctx, token)
		return nil, ErrNotFound
	}
	return &u, nil
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hashToken(token))
	return err
}

func (s *Store) PurgeExpiredSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, time.Now().UTC())
	return err
}

// ConstantTimeEqual is used for the optional shared API token.
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
