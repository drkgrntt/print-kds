package controllers

import (
	"errors"
	"strconv"
	"strings"

	"print-kds/internal/auth"
	"print-kds/internal/board"
	"print-kds/internal/logger"
	"print-kds/internal/server/middleware"
	"print-kds/internal/views"

	"github.com/gofiber/fiber/v2"
)

type UsersController struct {
	store     *auth.Store
	customers *board.Store // for the client-login customer picker
}

func NewUsersController(store *auth.Store, customers *board.Store) *UsersController {
	return &UsersController{store: store, customers: customers}
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
	if role != auth.RoleAdmin && role != auth.RoleClient {
		role = auth.RoleStaff
	}
	customerID, _ := strconv.ParseInt(c.FormValue("customer_id"), 10, 64)

	if msg := validateNewUser(email, password); msg != "" {
		return u.render(c, msg)
	}

	var err error
	if role == auth.RoleClient {
		if customerID == 0 {
			return u.render(c, "Pick the customer this client login belongs to.")
		}
		_, err = u.store.CreateClientUser(c.Context(), tenantID, customerID, email, password)
	} else {
		_, err = u.store.CreateUser(c.Context(), tenantID, email, password, role)
	}
	if err != nil {
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

	customers, err := u.customers.ListCustomers(c.Context(), middleware.CurrentTenantID(c))
	if err != nil {
		logger.Error("list customers", "error", err)
	}

	return render(c, views.UsersPage(views.UsersPageData{
		Users:         users,
		Customers:     customers,
		Error:         errMsg,
		CurrentUserID: middleware.CurrentUserID(c),
		Layout:        views.LayoutData{TenantName: middleware.CurrentTenantName(c), IsAdmin: true},
	}))
}
