package middleware

import (
	"print-kds/internal/auth"

	"github.com/gofiber/fiber/v2"
)

// SessionCookieName is the cookie the login/signup handlers set and
// RequireAuth reads back on every subsequent request.
const SessionCookieName = "pkds_session"

// RequireAuth resolves the session cookie into a tenant/user/role and
// stores them on the request's Locals for downstream handlers - see
// CurrentTenantID/CurrentTenantName/CurrentUserID/CurrentRole/IsAdmin.
// Registered once in routes.go, ahead of every controller except the
// public auth routes (login/signup/logout) and static/health.
func RequireAuth(authStore *auth.Store) fiber.Handler {
	return func(c *fiber.Ctx) error {
		token := c.Cookies(SessionCookieName)
		if token == "" {
			return redirectToLogin(c)
		}

		session, err := authStore.ValidateSession(c.Context(), token)
		if err != nil {
			return redirectToLogin(c)
		}

		c.Locals("user_id", session.UserID)
		c.Locals("tenant_id", session.TenantID)
		c.Locals("tenant_name", session.TenantName)
		c.Locals("role", session.Role)
		return c.Next()
	}
}

// RequireAdmin gates a route group to RoleAdmin users - it must run after
// RequireAuth on the same route (it reads Locals RequireAuth sets).
// Applied only where needed (order intake, user management), not the
// whole board - see routes.go.
func RequireAdmin(c *fiber.Ctx) error {
	if !IsAdmin(c) {
		return c.Status(fiber.StatusForbidden).SendString("admins only")
	}
	return c.Next()
}

// redirectToLogin sends a session-less/expired request to /login. A plain
// redirect doesn't compose with the board's htmx endpoints, which reply
// with hx-swap="none" plus out-of-band fragments rather than a normal
// document response (see docs/architecture.md's OOB-swap section) - htmx
// sets the HX-Request header on every request it issues, so that's used
// to pick htmx's own client-side redirect header instead of an HTTP
// redirect for those requests.
func redirectToLogin(c *fiber.Ctx) error {
	if c.Get("HX-Request") == "true" {
		c.Set("HX-Redirect", "/login")
		return c.SendStatus(fiber.StatusOK)
	}
	return c.Redirect("/login", fiber.StatusSeeOther)
}

func CurrentTenantID(c *fiber.Ctx) int64 {
	id, _ := c.Locals("tenant_id").(int64)
	return id
}

func CurrentTenantName(c *fiber.Ctx) string {
	name, _ := c.Locals("tenant_name").(string)
	return name
}

func CurrentUserID(c *fiber.Ctx) int64 {
	id, _ := c.Locals("user_id").(int64)
	return id
}

func CurrentRole(c *fiber.Ctx) auth.Role {
	role, _ := c.Locals("role").(auth.Role)
	return role
}

func IsAdmin(c *fiber.Ctx) bool {
	return CurrentRole(c) == auth.RoleAdmin
}
