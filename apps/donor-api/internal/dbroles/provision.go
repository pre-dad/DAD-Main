package dbroles

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Login is the database user a process connects as, read from its URL.
type Login struct {
	Role     Role
	User     string
	Password string
	Database string
	// Pool is the pool size the URL's pool_max_conns sets, or the role's.
	Pool int32
	// Replicas is how many instances of the process may run at once; zero is one.
	Replicas int32
}

// ConnectionLimit is the login's CONNECTION LIMIT: every replica's pool.
func (l Login) ConnectionLimit() int32 {
	return l.Pool * max(l.Replicas, 1)
}

// minPassword keeps logins out of reach of guessing; generated passwords
// such as `openssl rand -hex 32` are far longer.
const minPassword = 16

// ParseLogin reads role's login from the process's database URL.
func ParseLogin(role Role, url string) (Login, error) {
	config, err := pgx.ParseConfig(url)
	if err != nil {
		return Login{}, fmt.Errorf("%s is not a valid database URL", role.Variable())
	}
	login := Login{Role: role, User: config.User, Password: config.Password, Database: config.Database, Pool: role.Pool, Replicas: 1}
	if size := config.RuntimeParams["pool_max_conns"]; size != "" {
		limit, err := strconv.ParseInt(size, 10, 32)
		if err != nil || limit < 1 {
			return Login{}, fmt.Errorf("%s has an invalid pool_max_conns", role.Variable())
		}
		login.Pool = int32(limit)
	}
	switch {
	case login.User == "" || strings.HasPrefix(login.User, "pg_") || slices.ContainsFunc(Roles, func(r Role) bool { return r.Name == login.User }):
		return Login{}, fmt.Errorf("%s must name its own login user, not a group role", role.Variable())
	// SCRAM would need SASLprep for other characters; printable ASCII needs none.
	case len(login.Password) < minPassword || strings.ContainsFunc(login.Password, func(r rune) bool { return r <= ' ' || r > '~' }):
		return Login{}, fmt.Errorf("%s needs a password of at least %d printable ASCII characters", role.Variable(), minPassword)
	}
	return login, nil
}

// Provision creates or updates each login: its password, its connection
// limit, and its membership of its role. It refuses when a role's privileges
// reach any other user, such as a stale login or another environment's, so
// each role has exactly the one login its process uses.
func Provision(ctx context.Context, tx pgx.Tx, logins []Login) error {
	var owner string
	if err := tx.QueryRow(ctx, "SELECT current_user").Scan(&owner); err != nil {
		return err
	}
	users := map[string]Role{}
	for _, login := range logins {
		if login.User == owner {
			return fmt.Errorf("%s names the schema owner; each process needs its own login", login.Role.Variable())
		}
		if other, taken := users[login.User]; taken {
			return fmt.Errorf("%s and %s name the same user; each process needs its own login", other.Variable(), login.Role.Variable())
		}
		users[login.User] = login.Role
	}
	for _, login := range logins {
		if err := provision(ctx, tx, login); err != nil {
			return err
		}
	}
	// PostgreSQL 16 records whether a membership inherits or may SET ROLE. Before
	// it, every membership does both, so every member reaches the role.
	reaches := "true"
	var perMembership bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid='pg_auth_members'::regclass AND attname='inherit_option')").
		Scan(&perMembership); err != nil {
		return err
	}
	if perMembership {
		reaches = "(a.inherit_option OR a.set_option)"
	}
	for _, login := range logins {
		rows, err := tx.Query(ctx, `SELECT m.rolname FROM pg_auth_members a JOIN pg_roles m ON m.oid=a.member
WHERE a.roleid=(SELECT oid FROM pg_roles WHERE rolname=$1) AND `+reaches+` AND m.rolname<>$2 ORDER BY m.rolname`, login.Role.Name, login.User)
		if err != nil {
			return err
		}
		others, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if len(others) > 0 {
			return fmt.Errorf(
				"%s's privileges also reach %s; revoke the role from each (REVOKE %s FROM …) or drop it",
				login.Role.Name,
				strings.Join(others, ", "),
				login.Role.Name,
			)
		}
	}
	return nil
}

func provision(ctx context.Context, tx pgx.Tx, login Login) error {
	verifier, err := scramVerifier(login.Password)
	if err != nil {
		return err
	}
	user := pgx.Identifier{login.User}.Sanitize()
	var elevated bool
	err = tx.QueryRow(ctx, `SELECT rolsuper OR rolcreaterole OR rolcreatedb OR rolreplication OR rolbypassrls OR NOT rolinherit FROM pg_roles WHERE rolname=$1`, login.User).
		Scan(&elevated)
	var statement string
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		statement = "CREATE ROLE " + user
	case err != nil:
		return err
	case elevated:
		return fmt.Errorf("login %s must be a plain login that inherits its role's privileges", login.User)
	default:
		statement = "ALTER ROLE " + user
	}
	// The verifier, never the password, reaches the server and its logs.
	statement += fmt.Sprintf(" WITH LOGIN CONNECTION LIMIT %d PASSWORD '%s'", login.ConnectionLimit(), verifier)
	if _, err = tx.Exec(ctx, statement); err != nil {
		return fmt.Errorf("create or update login %s (the owner needs CREATEROLE): %w", login.User, err)
	}
	if _, err = tx.Exec(ctx, "GRANT "+pgx.Identifier{login.Role.Name}.Sanitize()+" TO "+user); err != nil {
		return fmt.Errorf("grant %s to %s: %w", login.Role.Name, login.User, err)
	}
	return nil
}

// scramVerifier returns PostgreSQL's SCRAM-SHA-256 verifier for password, as
// psql's \password computes it, so the password itself is never sent.
func scramVerifier(password string) (string, error) {
	const iterations = 4096
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	salted, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		return "", err
	}
	keyed := func(message string) []byte {
		mac := hmac.New(sha256.New, salted)
		mac.Write([]byte(message))
		return mac.Sum(nil)
	}
	stored := sha256.Sum256(keyed("Client Key"))
	encode := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iterations, encode(salt), encode(stored[:]), encode(keyed("Server Key"))), nil
}
