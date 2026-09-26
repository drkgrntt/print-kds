package controllers

import (
	"errors"
	"strconv"
	"strings"

	"print-kds/internal/auth"
	"print-kds/internal/logger"
	"print-kds/internal/server/middleware"
	"print-kds/internal/views"

	"github.com/gofiber/fiber/v2"
)

type UsersController struct {
	store *auth.Store
}

func NewUsersController(store *auth.Store) *UsersController {
	return &UsersController{store: store}
}

// RegisterRoutes registers each route with middleware.RequireAdmin ahead
// of its handler, the same way NewOrderController gates order intake -
// user management is an admin-only area.
func (u *UsersController) RegisterRoutes(app *fiber.App) {
	app.Get("/admin/users", middleware.RequireAdmin, u.handleList)
	app.Post("/admin/users", middleware.RequireAdmin, u.handleCreate)
	app.Post("/admin/users/:id/delete", middleware.RequireAdmin, u.handleDelete)
}

func (u *UsersController) handleList(c *fiber.Ctx) error {
	return u.render(c, "")
}

func (u *UsersController) handleCreate(c *fiber.Ctx) error {
	tenantID := middleware.CurrentTenantID(c)
	email := strings.TrimSpace(c.FormValue("email"))
	password := c.FormValue("password")
	role := auth.Role(c.FormValue("role"))
	if role != auth.RoleAdmin {
		role = auth.RoleStaff
	}

	if msg := validateNewUser(email, password); msg != "" {
		return u.render(c, msg)
	}

	if _, err := u.store.CreateUser(c.Context(), tenantID, email, password, role); err != nil {
		if errors.Is(err, auth.ErrDuplicateEmail) {
			return u.render(c, "That email is already registered.")
		}
		logger.Error("create user", "error", err)
		return u.render(c, "Something went wrong adding that user - try again.")
	}

	return c.Redirect("/admin/users", fiber.StatusSeeOther)
}

func validateNewUser(email, password string) string {
	switch {
	case email == "":
		return "Email is required."
	case len(password) < minPasswordLength:
		return "Password must be at least 8 characters."
	}
	return ""
}

func (u *UsersController) handleDelete(c *fiber.Ctx) error {
	tenantID := middleware.CurrentTenantID(c)
	userID, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).SendString("invalid user id")
	}

	if err := u.store.DeleteUser(c.Context(), tenantID, userID); err != nil {
		if errors.Is(err, auth.ErrLastAdmin) {
			return u.render(c, "Can't remove the last admin for this shop.")
		}
		logger.Error("delete user", "error", err)
		return u.render(c, "Something went wrong removing that user - try again.")
	}

	return c.Redirect("/admin/users", fiber.StatusSeeOther)
}

func (u *UsersController) render(c *fiber.Ctx, errMsg string) error {
	users, err := u.store.ListUsers(c.Context(), middleware.CurrentTenantID(c))
	if err != nil {
		logger.Error("list users", "error", err)
		return c.Status(fiber.StatusInternalServerError).SendString("failed to load users")
	}

	return render(c, views.UsersPage(views.UsersPageData{
		Users:         users,
		Error:         errMsg,
		CurrentUserID: middleware.CurrentUserID(c),
		Layout:        views.LayoutData{TenantName: middleware.CurrentTenantName(c), IsAdmin: true},
	}))
}
