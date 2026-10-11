package controllers

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"print-kds/internal/board"
	"print-kds/internal/logger"
	"print-kds/internal/server/middleware"
	"print-kds/internal/views"

	"github.com/gofiber/fiber/v2"
)

// defaultInvoiceWindowDays is the "past 2 weeks" the batch form opens with.
const defaultInvoiceWindowDays = 14

type InvoicesController struct {
	store *board.Store
}

func NewInvoicesController(store *board.Store) *InvoicesController {
	return &InvoicesController{store: store}
}

// RegisterRoutes gates every route with middleware.RequireAdmin, like the
// other admin areas.
func (ic *InvoicesController) RegisterRoutes(app *fiber.App) {
	app.Get("/admin/invoices", middleware.RequireAdmin, ic.handlePage)
	app.Post("/admin/invoices", middleware.RequireAdmin, ic.handleCreate)
	app.Post("/admin/invoices/items/:id/price", middleware.RequireAdmin, ic.handleSetPrice)
	app.Get("/admin/invoices/:id", middleware.RequireAdmin, ic.handleShow)
}

// batchParams reads the date range + customer filter from either the query
// (preview) or the form body (create / price). ok is false when the dates
// are missing or malformed.
func batchParams(c *fiber.Ctx) (from, to time.Time, customerID int64, ok bool) {
	get := func(k string) string {
		if v := c.FormValue(k); v != "" {
			return v
		}
		return c.Query(k)
	}
	var err1, err2 error
	from, err1 = time.ParseInLocation(dueDateLayout, get("from"), time.Local)
	to, err2 = time.ParseInLocation(dueDateLayout, get("to"), time.Local)
	customerID, _ = strconv.ParseInt(get("customer_id"), 10, 64)
	return from, to, customerID, err1 == nil && err2 == nil && !to.Before(from)
}

func (ic *InvoicesController) page(c *fiber.Ctx, data views.InvoicesPageData) error {
	tenantID := middleware.CurrentTenantID(c)
	var err error
	if data.Invoices, err = ic.store.ListInvoices(c.Context(), tenantID); err != nil {
		logger.Error("list invoices", "error", err)
		return c.Status(fiber.StatusInternalServerError).SendString("failed to load invoices")
	}
	if data.Customers, err = ic.store.ListCustomers(c.Context(), tenantID); err != nil {
		logger.Error("list customers", "error", err)
	}
	data.Layout = views.LayoutData{TenantName: middleware.CurrentTenantName(c), IsAdmin: true}
	return render(c, views.InvoicesPage(data))
}

func (ic *InvoicesController) handlePage(c *fiber.Ctx) error {
	data := views.InvoicesPageData{
		From:    time.Now().AddDate(0, 0, -defaultInvoiceWindowDays).Format(dueDateLayout),
		To:      time.Now().Format(dueDateLayout),
		Message: c.Query("message"),
	}
	if c.Query("from") == "" && c.Query("to") == "" {
		return ic.page(c, data)
	}

	from, to, customerID, ok := batchParams(c)
	data.From, data.To, data.CustomerID = c.Query("from"), c.Query("to"), customerID
	if !ok {
		data.Error = "Enter a valid date range (the end can't be before the start)."
		return ic.page(c, data)
	}
	cands, err := ic.store.PreviewBatch(c.Context(), middleware.CurrentTenantID(c), customerID, from, to)
	if err != nil {
		logger.Error("preview invoice batch", "error", err)
		data.Error = "Something went wrong loading the preview - try again."
		return ic.page(c, data)
	}
	data.Previewed, data.Candidates = true, cands
	return ic.page(c, data)
}

func (ic *InvoicesController) handleCreate(c *fiber.Ctx) error {
	from, to, customerID, ok := batchParams(c)
	if !ok {
		return c.Redirect("/admin/invoices", fiber.StatusSeeOther)
	}
	res, err := ic.store.CreateBatch(c.Context(), middleware.CurrentTenantID(c), middleware.CurrentUserID(c), customerID, from, to)
	if err != nil {
		if !errors.Is(err, board.ErrItemInvoiced) {
			logger.Error("create invoice batch", "error", err)
		}
		return ic.page(c, views.InvoicesPageData{
			From: c.FormValue("from"), To: c.FormValue("to"), CustomerID: customerID,
			Error: "Couldn't create the invoices (someone may have just invoiced these items) - preview again.",
		})
	}

	msg := fmt.Sprintf("Created %d invoice(s).", len(res.Invoices))
	if res.Unpriced > 0 {
		msg += fmt.Sprintf(" %d unpriced item(s) were left out.", res.Unpriced)
	}
	if len(res.Invoices) == 1 {
		return c.Redirect(fmt.Sprintf("/admin/invoices/%d", res.Invoices[0].ID), fiber.StatusSeeOther)
	}
	return c.Redirect("/admin/invoices?message="+url.QueryEscape(msg), fiber.StatusSeeOther)
}

func (ic *InvoicesController) handleSetPrice(c *fiber.Ctx) error {
	itemID, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).SendString("invalid item id")
	}
	back := fmt.Sprintf("/admin/invoices?from=%s&to=%s&customer_id=%s",
		url.QueryEscape(c.FormValue("from")), url.QueryEscape(c.FormValue("to")), url.QueryEscape(c.FormValue("customer_id")))

	dollars, err := strconv.ParseFloat(strings.TrimSpace(c.FormValue("price")), 64)
	if err != nil || dollars < 0 || math.IsNaN(dollars) || math.IsInf(dollars, 0) {
		return c.Redirect(back, fiber.StatusSeeOther)
	}
	cents := int64(math.Round(dollars * 100))
	if err := ic.store.SetItemPrice(c.Context(), middleware.CurrentTenantID(c), itemID, cents); err != nil {
		if errors.Is(err, board.ErrItemNotFound) {
			return c.SendStatus(fiber.StatusNotFound)
		}
		if !errors.Is(err, board.ErrItemInvoiced) {
			logger.Error("set item price", "error", err)
		}
	}
	return c.Redirect(back, fiber.StatusSeeOther)
}

func (ic *InvoicesController) handleShow(c *fiber.Ctx) error {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).SendString("invalid invoice id")
	}
	inv, err := ic.store.GetInvoice(c.Context(), middleware.CurrentTenantID(c), id)
	if err != nil {
		if errors.Is(err, board.ErrInvoiceNotFound) {
			return c.SendStatus(fiber.StatusNotFound)
		}
		logger.Error("load invoice", "error", err)
		return c.Status(fiber.StatusInternalServerError).SendString("failed to load invoice")
	}
	return render(c, views.InvoicePage(inv, middleware.CurrentTenantName(c)))
}
