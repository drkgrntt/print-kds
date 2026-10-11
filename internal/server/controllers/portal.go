package controllers

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"print-kds/internal/board"
	"print-kds/internal/logger"
	"print-kds/internal/server/middleware"
	"print-kds/internal/views"

	"github.com/gofiber/fiber/v2"
)

// PortalController is the client-facing area: a customer's own order list
// and order form. Every handler scopes by the session's tenant and
// customer (middleware.CurrentTenantID/CurrentCustomerID) - never by a
// form field or URL param.
type PortalController struct {
	store     *board.Store
	uploadDir string
}

func NewPortalController(store *board.Store, uploadDir string) *PortalController {
	return &PortalController{store: store, uploadDir: uploadDir}
}

func (p *PortalController) RegisterRoutes(app *fiber.App) {
	app.Get("/portal", middleware.RequireClient, p.handleList)
	app.Get("/portal/new-order", middleware.RequireClient, p.handleForm)
	app.Post("/portal/new-order", middleware.RequireClient, p.handleCreate)
	app.Get("/portal/new-order/item-row", middleware.RequireClient, p.handleItemRow)
	app.Get("/portal/files/:id", middleware.RequireClient, p.handleClientFile)
	// Staff download the same files to slice/print them.
	app.Get("/files/:id", middleware.RequireStaff, p.handleStaffFile)
}

func portalLayout(c *fiber.Ctx) views.LayoutData {
	return views.LayoutData{TenantName: middleware.CurrentTenantName(c), IsClient: true}
}

func (p *PortalController) handleList(c *fiber.Ctx) error {
	orders, err := p.store.ListCustomerOrders(c.Context(), middleware.CurrentTenantID(c), middleware.CurrentCustomerID(c))
	if err != nil {
		logger.Error("list customer orders", "error", err)
		return c.Status(fiber.StatusInternalServerError).SendString("failed to load orders")
	}
	return render(c, views.PortalPage(views.PortalPageData{
		Orders:       orders,
		CreatedOrder: c.Query("created"),
		Layout:       portalLayout(c),
	}))
}

func (p *PortalController) formData(c *fiber.Ctx, f newOrderForm, errMsg string) views.PortalNewOrderData {
	d := f.toFormData(errMsg)
	return views.PortalNewOrderData{
		ShipBy: f.DueDate,
		ShipTo: f.ShipTo,
		Placed: time.Now().Format(dueDateLayout),
		Items:  d.Items,
		Error:  errMsg,
		Layout: portalLayout(c),
	}
}

func (p *PortalController) handleForm(c *fiber.Ctx) error {
	// Repeat clients usually ship to the same place: prefill from their
	// last order's address.
	shipTo, err := p.store.LastShipTo(c.Context(), middleware.CurrentTenantID(c), middleware.CurrentCustomerID(c))
	if err != nil {
		logger.Error("load last ship-to", "error", err)
	}
	return render(c, views.PortalNewOrder(p.formData(c, newOrderForm{ShipTo: shipTo}, "")))
}

func (p *PortalController) handleItemRow(c *fiber.Ctx) error {
	return render(c, views.ItemRow(views.NewOrderItemRow{}))
}

func (p *PortalController) handleCreate(c *fiber.Ctx) error {
	tenantID := middleware.CurrentTenantID(c)
	form := parseNewOrderForm(c)
	form.CustomerID = middleware.CurrentCustomerID(c) // from the session, never the form

	if msg := form.validateDateAndItems(); msg != "" {
		return render(c, views.PortalNewOrder(p.formData(c, form, msg)))
	}

	files, cleanup, err := saveUploads(c, p.uploadDir, tenantID)
	if err != nil {
		if errors.Is(err, ErrBadUpload) {
			return render(c, views.PortalNewOrder(p.formData(c, form, err.Error())))
		}
		logger.Error("save uploads", "error", err)
		return render(c, views.PortalNewOrder(p.formData(c, form, "Something went wrong uploading your files - try again.")))
	}

	in := form.toNewOrder()
	in.Files = files

	// The shop's order numbers are theirs to pick; a client's get a
	// generated one. A collision is vanishingly unlikely, so retry once.
	var orderNumber string
	for attempt := 0; attempt < 2; attempt++ {
		orderNumber = generateOrderNumber("WEB")
		in.OrderNumber = orderNumber
		_, err = p.store.CreateOrder(c.Context(), tenantID, middleware.CurrentUserID(c), in)
		if !errors.Is(err, board.ErrDuplicateOrderNumber) {
			break
		}
	}
	if err != nil {
		cleanup()
		logger.Error("create portal order", "error", err)
		return render(c, views.PortalNewOrder(p.formData(c, form, "Something went wrong placing your order - try again.")))
	}

	return c.Redirect("/portal?created="+orderNumber, fiber.StatusSeeOther)
}

func generateOrderNumber(prefix string) string {
	raw := make([]byte, 3)
	_, _ = rand.Read(raw)
	return fmt.Sprintf("%s-%s-%s", prefix, time.Now().Format("20060102"), strings.ToUpper(hex.EncodeToString(raw)))
}

func (p *PortalController) handleClientFile(c *fiber.Ctx) error {
	return p.sendFile(c, middleware.CurrentCustomerID(c))
}

func (p *PortalController) handleStaffFile(c *fiber.Ctx) error {
	return p.sendFile(c, 0)
}

// sendFile serves one order file after a tenant check (and, for clients, a
// customer check) in GetOrderFile - a miss for either is a plain 404 so
// another shop's/customer's file ids can't be probed.
func (p *PortalController) sendFile(c *fiber.Ctx, customerID int64) error {
	fileID, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).SendString("invalid file id")
	}
	f, err := p.store.GetOrderFile(c.Context(), middleware.CurrentTenantID(c), customerID, fileID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return c.SendStatus(fiber.StatusNotFound)
		}
		logger.Error("load order file", "error", err)
		return c.SendStatus(fiber.StatusInternalServerError)
	}
	return c.Download(f.StoredPath, f.Filename)
}
