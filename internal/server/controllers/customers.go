package controllers

import (
	"errors"
	"strings"

	"print-kds/internal/board"
	"print-kds/internal/logger"
	"print-kds/internal/server/middleware"
	"print-kds/internal/views"

	"github.com/gofiber/fiber/v2"
)

type CustomersController struct {
	store *board.Store
}

func NewCustomersController(store *board.Store) *CustomersController {
	return &CustomersController{store: store}
}

func (cc *CustomersController) RegisterRoutes(app *fiber.App) {
	app.Get("/admin/customers", middleware.RequireAdmin, cc.handleList)
	app.Post("/admin/customers", middleware.RequireAdmin, cc.handleCreate)
}

func (cc *CustomersController) handleList(c *fiber.Ctx) error {
	return cc.render(c, "")
}

func (cc *CustomersController) handleCreate(c *fiber.Ctx) error {
	cust := board.Customer{
		Name:           strings.TrimSpace(c.FormValue("name")),
		BillingAddress: strings.TrimSpace(c.FormValue("billing_address")),
		Email:          strings.TrimSpace(c.FormValue("email")),
	}
	if cust.Name == "" {
		return cc.render(c, "Customer name is required.")
	}
	if _, err := cc.store.CreateCustomer(c.Context(), middleware.CurrentTenantID(c), cust); err != nil {
		if errors.Is(err, board.ErrDuplicateCustomer) {
			return cc.render(c, "A customer with that name already exists.")
		}
		logger.Error("create customer", "error", err)
		return cc.render(c, "Something went wrong adding that customer - try again.")
	}
	return c.Redirect("/admin/customers", fiber.StatusSeeOther)
}

func (cc *CustomersController) render(c *fiber.Ctx, errMsg string) error {
	customers, err := cc.store.ListCustomers(c.Context(), middleware.CurrentTenantID(c))
	if err != nil {
		logger.Error("list customers", "error", err)
		return c.Status(fiber.StatusInternalServerError).SendString("failed to load customers")
	}
	return render(c, views.CustomersPage(views.CustomersPageData{
		Customers: customers,
		Error:     errMsg,
		Layout:    views.LayoutData{TenantName: middleware.CurrentTenantName(c), IsAdmin: true},
	}))
}
