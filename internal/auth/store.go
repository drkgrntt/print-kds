// Package auth is the domain layer for tenants, users, and sessions: it
// mirrors internal/board's Store pattern (one Store wrapping the shared
// *sql.DB, raw SQL, no ORM) rather than introducing a second style for
// login/session concerns.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"
)

// ErrInvalidCredentials is returned by Authenticate for both "no such
// email" and "wrong password" alike - callers must never distinguish the
// two in a user-facing message, since doing so would let an attacker
// enumerate registered emails.
var ErrInvalidCredentials = errors.New("invalid email or password")

// ErrDuplicateEmail is wrapped into the error SignUp/CreateUser return
// when email's unique constraint rejects the insert.
var ErrDuplicateEmail = errors.New("email already registered")

// ErrSessionNotFound is returned by ValidateSession when token doesn't
// match a live, non-expired session row.
var ErrSessionNotFound = errors.New("session not found or expired")

// ErrLastAdmin is returned by DeleteUser when userID is the tenant's only
// remaining admin - refusing the delete rather than locking the shop out
// of its own user management.
var ErrLastAdmin = errors.New("cannot remove the last admin for this shop")

// sessionTTL is a fixed session lifetime - no sliding refresh in this
// pass, a session is just re-created by logging in again once it expires.
const sessionTTL = 30 * 24 * time.Hour

// Role gates access to admin-only areas (order intake, user management -
// see internal/server/middleware.RequireAdmin), ordinary board operation
// (admin and staff - see middleware.RequireStaff), and the client portal
// (RoleClient only - a customer's own login, tied to one customers row).
type Role string

const (
	RoleAdmin  Role = "admin"
	RoleStaff  Role = "staff"
	RoleClient Role = "client"
)

// ErrClientNeedsCustomer is returned when a client user is created without
// a customer, or a non-client user is created with one.
var ErrClientNeedsCustomer = errors.New("client users must belong to a customer, and only client users can")

// User mirrors a users row.
type User struct {
	ID       int64
	TenantID int64
	Email    string
	Role     Role
	// CustomerID is set only for RoleClient users.
	CustomerID int64
}

// Session is what a valid session cookie resolves to - joined against
// users and tenants so TenantName and Role are always current as of this
// request, not stale from whenever the session was created (e.g. a role
// change or a tenant rename takes effect on the very next request).
type Session struct {
	UserID     int64
	TenantID   int64
	TenantName string
	Role       Role
	CustomerID int64 // 0 unless Role is RoleClient
	ExpiresAt  time.Time
}

type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// SignUp creates a brand-new tenant and its first user, always as
// RoleAdmin (the shop owner) - the only way a tenant comes into being
// through the app itself. An already-existing tenant gaining an
// additional login goes through CreateUser instead (see
// cmd/createuser for provisioning a login onto a tenant that predates
// self-serve signup, e.g. the migration-backfilled "Default Shop").
func (s *Store) SignUp(ctx context.Context, tenantName, email, password string) (User, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return User{}, fmt.Errorf("hash password: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	var tenantID int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO tenants (name) VALUES ($1) RETURNING id`, tenantName,
	).Scan(&tenantID); err != nil {
		return User{}, fmt.Errorf("insert tenant: %w", err)
	}

	email = normalizeEmail(email)
	user := User{TenantID: tenantID, Email: email, Role: RoleAdmin}
	if err := tx.QueryRowContext(ctx, `
INSERT INTO users (tenant_id, email, password_hash, role)
VALUES ($1, $2, $3, $4)
RETURNING id`,
		tenantID, email, string(hash), string(RoleAdmin),
	).Scan(&user.ID); err != nil {
		if isUniqueViolation(err) {
			return User{}, fmt.Errorf("%w: %q", ErrDuplicateEmail, email)
		}
		return User{}, fmt.Errorf("insert user: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return User{}, fmt.Errorf("commit sign up: %w", err)
	}
	return user, nil
}

// Authenticate looks up email and bcrypt-compares password against its
// stored hash. Both "no such email" and "wrong password" return the same
// ErrInvalidCredentials so a caller can't distinguish - and must not try
// to, in whatever message it shows - which one failed.
func (s *Store) Authenticate(ctx context.Context, email, password string) (User, error) {
	email = normalizeEmail(email)

	var user User
	var hash string
	var customerID sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, email, password_hash, role, customer_id FROM users WHERE email = $1`,
		email,
	).Scan(&user.ID, &user.TenantID, &user.Email, &hash, &user.Role, &customerID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrInvalidCredentials
		}
		return User{}, fmt.Errorf("load user %q: %w", email, err)
	}

	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return User{}, ErrInvalidCredentials
	}
	user.CustomerID = customerID.Int64
	return user, nil
}

// CreateSession mints a random token, storing only its SHA-256 hash so a
// leaked database backup can't be replayed as a live session cookie. The
// raw token is returned once, for the caller (the login/signup handler)
// to set as the cookie value - it's never persisted or logged.
func (s *Store) CreateSession(ctx context.Context, user User) (token string, expiresAt time.Time, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, fmt.Errorf("generate session token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	tokenHash := hashToken(token)
	expiresAt = time.Now().Add(sessionTTL)

	if _, err := s.db.ExecContext(ctx, `
INSERT INTO sessions (token_hash, user_id, tenant_id, expires_at)
VALUES ($1, $2, $3, $4)`,
		tokenHash, user.ID, user.TenantID, expiresAt,
	); err != nil {
		return "", time.Time{}, fmt.Errorf("insert session: %w", err)
	}
	return token, expiresAt, nil
}

// ValidateSession hashes token and joins to users/tenants for a live
// (non-expired) session - see Session's doc comment for why TenantName
// and Role are read fresh here rather than denormalized onto the
// sessions row at creation time.
func (s *Store) ValidateSession(ctx context.Context, token string) (Session, error) {
	var sess Session
	var customerID sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
SELECT s.user_id, s.tenant_id, t.name, u.role, u.customer_id, s.expires_at
FROM sessions s
JOIN users u ON u.id = s.user_id
JOIN tenants t ON t.id = s.tenant_id
WHERE s.token_hash = $1 AND s.expires_at > now()`,
		hashToken(token),
	).Scan(&sess.UserID, &sess.TenantID, &sess.TenantName, &sess.Role, &customerID, &sess.ExpiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Session{}, ErrSessionNotFound
		}
		return Session{}, fmt.Errorf("load session: %w", err)
	}
	sess.CustomerID = customerID.Int64
	return sess, nil
}

// DeleteSession removes token's row. Deleting a nonexistent token is not
// an error - a double logout (e.g. two tabs) is harmless.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = $1`, hashToken(token)); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ListUsers returns tenantID's users ordered by email, for the
// /admin/users management page.
func (s *Store) ListUsers(ctx context.Context, tenantID int64) ([]User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tenant_id, email, role, customer_id FROM users WHERE tenant_id = $1 ORDER BY email`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("query users for tenant %d: %w", tenantID, err)
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var u User
		var customerID sql.NullInt64
		if err := rows.Scan(&u.ID, &u.TenantID, &u.Email, &u.Role, &customerID); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		u.CustomerID = customerID.Int64
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate users: %w", err)
	}
	return users, nil
}

// CreateUser adds an admin/staff teammate to an already-existing tenant -
// the admin-driven counterpart to SignUp, which instead creates a
// brand-new tenant. Returns ErrDuplicateEmail the same way SignUp does.
// Client logins go through CreateClientUser.
func (s *Store) CreateUser(ctx context.Context, tenantID int64, email, password string, role Role) (User, error) {
	if role == RoleClient {
		return User{}, ErrClientNeedsCustomer
	}
	return s.createUser(ctx, tenantID, 0, email, password, role)
}

// CreateClientUser adds a login for customerID's company. The customer is
// verified to belong to tenantID, so a client can never be attached to
// another shop's customer.
func (s *Store) CreateClientUser(ctx context.Context, tenantID, customerID int64, email, password string) (User, error) {
	if customerID == 0 {
		return User{}, ErrClientNeedsCustomer
	}
	return s.createUser(ctx, tenantID, customerID, email, password, RoleClient)
}

func (s *Store) createUser(ctx context.Context, tenantID, customerID int64, email, password string, role Role) (User, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return User{}, fmt.Errorf("hash password: %w", err)
	}

	var cust sql.NullInt64
	if customerID != 0 {
		var ok bool
		if err := s.db.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM customers WHERE id = $1 AND tenant_id = $2)`,
			customerID, tenantID,
		).Scan(&ok); err != nil {
			return User{}, fmt.Errorf("check customer %d: %w", customerID, err)
		}
		if !ok {
			return User{}, fmt.Errorf("customer %d not found for tenant %d", customerID, tenantID)
		}
		cust = sql.NullInt64{Int64: customerID, Valid: true}
	}

	email = normalizeEmail(email)
	user := User{TenantID: tenantID, Email: email, Role: role, CustomerID: customerID}
	if err := s.db.QueryRowContext(ctx, `
INSERT INTO users (tenant_id, email, password_hash, role, customer_id)
VALUES ($1, $2, $3, $4, $5)
RETURNING id`,
		tenantID, email, string(hash), string(role), cust,
	).Scan(&user.ID); err != nil {
		if isUniqueViolation(err) {
			return User{}, fmt.Errorf("%w: %q", ErrDuplicateEmail, email)
		}
		return User{}, fmt.Errorf("insert user: %w", err)
	}
	return user, nil
}

// DeleteUser removes userID, scoped to tenantID in the WHERE clause so an
// admin can only ever delete a user in their own shop - the same
// tenant-scoped-lookup pattern board.Store.beginItemTxAny uses for print
// items. Refuses (ErrLastAdmin) if userID is the tenant's only remaining
// admin, rather than letting a shop lock itself out of user management.
func (s *Store) DeleteUser(ctx context.Context, tenantID, userID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	var role Role
	err = tx.QueryRowContext(ctx,
		`SELECT role FROM users WHERE id = $1 AND tenant_id = $2 FOR UPDATE`,
		userID, tenantID,
	).Scan(&role)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // already gone (or not this tenant's) - nothing to do
		}
		return fmt.Errorf("load user %d: %w", userID, err)
	}

	if role == RoleAdmin {
		var adminCount int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM users WHERE tenant_id = $1 AND role = $2`,
			tenantID, string(RoleAdmin),
		).Scan(&adminCount); err != nil {
			return fmt.Errorf("count admins for tenant %d: %w", tenantID, err)
		}
		if adminCount <= 1 {
			return ErrLastAdmin
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = $1 AND tenant_id = $2`, userID, tenantID); err != nil {
		return fmt.Errorf("delete user %d: %w", userID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete user %d: %w", userID, err)
	}
	return nil
}
