package auth

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"print-kds/internal/database"

	"github.com/joho/godotenv"
)

// init loads the repo-root .env explicitly, the same way
// internal/board/store_test.go does - godotenv/autoload only ever looks in
// the process's starting working directory, which for `go test` is this
// package's own source directory, not the repo root.
func init() {
	_ = godotenv.Load("../../.env")
}

// testStore opens a connection the same way database.New() does (applying
// migrations along the way) and hands back a *Store, so these tests
// exercise real Postgres rather than a mock.
func testStore(t *testing.T) *Store {
	t.Helper()
	svc := database.New()
	return NewStore(svc.DB())
}

func uniqueEmail(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%s-%d@example.com", t.Name(), time.Now().UnixNano())
}

// cleanupTenant deletes tenantID once the test finishes - cascades through
// users/sessions via ON DELETE CASCADE.
func cleanupTenant(t *testing.T, s *Store, tenantID int64) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := s.db.ExecContext(context.Background(), `DELETE FROM tenants WHERE id = $1`, tenantID); err != nil {
			t.Errorf("cleanup tenant %d: %v", tenantID, err)
		}
	})
}

func TestSignUp_DuplicateEmail(t *testing.T) {
	s := testStore(t)
	email := uniqueEmail(t)

	user, err := s.SignUp(context.Background(), "Shop A", email, "password123")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	cleanupTenant(t, s, user.TenantID)
	if user.Role != RoleAdmin {
		t.Errorf("role = %q, want %q", user.Role, RoleAdmin)
	}

	_, err = s.SignUp(context.Background(), "Shop B", email, "password456")
	if !errors.Is(err, ErrDuplicateEmail) {
		t.Fatalf("err = %v, want ErrDuplicateEmail", err)
	}
}

func TestAuthenticate(t *testing.T) {
	s := testStore(t)
	email := uniqueEmail(t)

	user, err := s.SignUp(context.Background(), "Shop A", email, "password123")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	cleanupTenant(t, s, user.TenantID)

	if _, err := s.Authenticate(context.Background(), email, "password123"); err != nil {
		t.Errorf("Authenticate with correct password: %v", err)
	}

	if _, err := s.Authenticate(context.Background(), email, "wrong-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("err = %v, want ErrInvalidCredentials for wrong password", err)
	}

	if _, err := s.Authenticate(context.Background(), "nobody-"+email, "password123"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("err = %v, want ErrInvalidCredentials for unknown email", err)
	}
}

func TestSession_CreateValidateDelete(t *testing.T) {
	s := testStore(t)
	email := uniqueEmail(t)

	user, err := s.SignUp(context.Background(), "Shop A", email, "password123")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	cleanupTenant(t, s, user.TenantID)

	token, _, err := s.CreateSession(context.Background(), user)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	session, err := s.ValidateSession(context.Background(), token)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}
	if session.UserID != user.ID || session.TenantID != user.TenantID {
		t.Errorf("session = %+v, want user %d/tenant %d", session, user.ID, user.TenantID)
	}
	if session.Role != RoleAdmin {
		t.Errorf("session role = %q, want %q", session.Role, RoleAdmin)
	}

	if err := s.DeleteSession(context.Background(), token); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := s.ValidateSession(context.Background(), token); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("err = %v, want ErrSessionNotFound after delete", err)
	}

	// A second delete of the same (now-gone) token is a no-op, not an error.
	if err := s.DeleteSession(context.Background(), token); err != nil {
		t.Errorf("second DeleteSession: %v, want nil", err)
	}
}

func TestValidateSession_Expired(t *testing.T) {
	s := testStore(t)
	email := uniqueEmail(t)

	user, err := s.SignUp(context.Background(), "Shop A", email, "password123")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	cleanupTenant(t, s, user.TenantID)

	token, _, err := s.CreateSession(context.Background(), user)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if _, err := s.db.ExecContext(context.Background(),
		`UPDATE sessions SET expires_at = now() - interval '1 minute' WHERE token_hash = $1`, hashToken(token),
	); err != nil {
		t.Fatalf("force-expire session: %v", err)
	}

	if _, err := s.ValidateSession(context.Background(), token); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("err = %v, want ErrSessionNotFound for an expired session", err)
	}
}

func TestCreateUser_ListUsers(t *testing.T) {
	s := testStore(t)
	owner, err := s.SignUp(context.Background(), "Shop A", uniqueEmail(t), "password123")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	cleanupTenant(t, s, owner.TenantID)

	staffEmail := uniqueEmail(t)
	staff, err := s.CreateUser(context.Background(), owner.TenantID, staffEmail, "password123", RoleStaff)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if staff.Role != RoleStaff {
		t.Errorf("role = %q, want %q", staff.Role, RoleStaff)
	}

	users, err := s.ListUsers(context.Background(), owner.TenantID)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("len(users) = %d, want 2", len(users))
	}

	if _, err := s.CreateUser(context.Background(), owner.TenantID, staffEmail, "password123", RoleStaff); !errors.Is(err, ErrDuplicateEmail) {
		t.Errorf("err = %v, want ErrDuplicateEmail", err)
	}
}

func TestDeleteUser_RefusesLastAdmin(t *testing.T) {
	s := testStore(t)
	owner, err := s.SignUp(context.Background(), "Shop A", uniqueEmail(t), "password123")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	cleanupTenant(t, s, owner.TenantID)

	if err := s.DeleteUser(context.Background(), owner.TenantID, owner.ID); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("err = %v, want ErrLastAdmin", err)
	}

	// Adding a second admin means the first can now be removed.
	second, err := s.CreateUser(context.Background(), owner.TenantID, uniqueEmail(t), "password123", RoleAdmin)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.DeleteUser(context.Background(), owner.TenantID, owner.ID); err != nil {
		t.Fatalf("DeleteUser after adding a second admin: %v", err)
	}

	users, err := s.ListUsers(context.Background(), owner.TenantID)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 1 || users[0].ID != second.ID {
		t.Errorf("users = %+v, want just %+v", users, second)
	}
}

func TestDeleteUser_WrongTenantIsNoop(t *testing.T) {
	s := testStore(t)
	ownerA, err := s.SignUp(context.Background(), "Shop A", uniqueEmail(t), "password123")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	cleanupTenant(t, s, ownerA.TenantID)

	ownerB, err := s.SignUp(context.Background(), "Shop B", uniqueEmail(t), "password123")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	cleanupTenant(t, s, ownerB.TenantID)

	if err := s.DeleteUser(context.Background(), ownerA.TenantID, ownerB.ID); err != nil {
		t.Fatalf("DeleteUser across tenants: %v, want nil (silent no-op)", err)
	}

	users, err := s.ListUsers(context.Background(), ownerB.TenantID)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 1 || users[0].ID != ownerB.ID {
		t.Errorf("shop B users = %+v, want owner B untouched", users)
	}
}
