package controllers

import (
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

// dueDateLayout is the format both <input type="date"> submits and expects
// back for its value attribute (see NewOrderFormData.DueDate).
const dueDateLayout = "2006-01-02"

// minItemRows is how many blank item rows the form always shows, even
// right after a validation error strips out empty ones - there should
// always be at least one empty row left to type into.
const minItemRows = 2

type NewOrderController struct {
	store *board.Store
}

func NewNewOrderController(store *board.Store) *NewOrderController {
	return &NewOrderController{store: store}
}

// RegisterRoutes registers each route with middleware.RequireAdmin ahead
// of its handler - it runs after routes.go's RequireAuth, so order intake
// is restricted to admins while the board itself (registered separately)
// stays open to any logged-in user.
func (n *NewOrderController) RegisterRoutes(app *fiber.App) {
	app.Get("/admin/new-order", middleware.RequireAdmin, n.handleForm)
	app.Post("/admin/new-order", middleware.RequireAdmin, n.handleCreate)
	app.Get("/admin/new-order/item-row", middleware.RequireAdmin, n.handleItemRow)
	app.Get("/admin/new-order/ship-to", middleware.RequireAdmin, n.handleShipTo)
}

func (n *NewOrderController) handleForm(c *fiber.Ctx) error {
	customers, err := n.store.ListCustomers(c.Context(), middleware.CurrentTenantID(c))
	if err != nil {
		logger.Error("list customers", "error", err)
	}
	data := views.NewOrderFormData{
		Items:              blankItemRows(minItemRows),
		CreatedOrderNumber: c.Query("created"),
		Customers:          customers,
	}
	layout := views.LayoutData{TenantName: middleware.CurrentTenantName(c), IsAdmin: middleware.IsAdmin(c)}
	return render(c, views.NewOrderForm(data, layout))
}

// handleShipTo serves the customer select's htmx request: the ship-to
// fields prefilled from that customer's most recent addressed order (blank
// for "one-off" or a customer with none). Tenant-scoped like every read.
func (n *NewOrderController) handleShipTo(c *fiber.Ctx) error {
	var shipTo board.ShipTo
	if customerID, _ := strconv.ParseInt(c.Query("customer_id"), 10, 64); customerID != 0 {
		var err error
		if shipTo, err = n.store.LastShipTo(c.Context(), middleware.CurrentTenantID(c), customerID); err != nil {
			logger.Error("load last ship-to", "error", err)
		}
	}
	return render(c, views.ShipToFields(shipTo))
}

// handleItemRow serves the "+ Add another item" button's htmx request: one
// blank item-row fragment, appended client-side (hx-swap="beforeend") into
// the form's #item-rows container - no JS, no index bookkeeping, since
// every row reuses the same input names and the server reads them back as
// parallel arrays (see parseNewOrderForm).
func (n *NewOrderController) handleItemRow(c *fiber.Ctx) error {
	return render(c, views.ItemRow(views.NewOrderItemRow{}))
}

func (n *NewOrderController) handleCreate(c *fiber.Ctx) error {
	form := parseNewOrderForm(c)
	layout := views.LayoutData{TenantName: middleware.CurrentTenantName(c), IsAdmin: middleware.IsAdmin(c)}
	customers, err := n.store.ListCustomers(c.Context(), middleware.CurrentTenantID(c))
	if err != nil {
		logger.Error("list customers", "error", err)
	}
	withCustomers := func(msg string) views.NewOrderFormData {
		d := form.toFormData(msg)
		d.Customers = customers
		return d
	}

	if msg := form.validate(); msg != "" {
		return render(c, views.NewOrderForm(withCustomers(msg), layout))
	}

	if _, err := n.store.CreateOrder(c.Context(), middleware.CurrentTenantID(c), middleware.CurrentUserID(c), form.toNewOrder()); err != nil {
		if errors.Is(err, board.ErrDuplicateOrderNumber) {
			return render(c, views.NewOrderForm(withCustomers(fmt.Sprintf("Order number %q already exists - pick a different one.", form.OrderNumber)), layout))
		}
		logger.Error("create order", "error", err)
		return render(c, views.NewOrderForm(withCustomers("Something went wrong saving this order - try again."), layout))
	}

	return c.Redirect(fmt.Sprintf("/admin/new-order?created=%s", form.OrderNumber), fiber.StatusSeeOther)
}

// newOrderForm holds one submission's raw values - item fields are
// parallel arrays (index i is one row) since the form repeats the same
// input names across every item row rather than indexing them.
type newOrderForm struct {
	OrderNumber    string
	CustomerID     int64
	CustomerName   string
	DueDate        string
	ShipTo         board.ShipTo
	FileNames      []string
	ColorMaterials []string
	Quantities     []string
}

func parseNewOrderForm(c *fiber.Ctx) newOrderForm {
	customerID, _ := strconv.ParseInt(c.FormValue("customer_id"), 10, 64)
	return newOrderForm{
		OrderNumber:  strings.TrimSpace(c.FormValue("order_number")),
		CustomerID:   customerID,
		CustomerName: strings.TrimSpace(c.FormValue("customer_name")),
		DueDate:      strings.TrimSpace(c.FormValue("due_date")),
		ShipTo: board.ShipTo{
			Name:       c.FormValue("ship_name"),
			Line1:      c.FormValue("ship_line1"),
			Line2:      c.FormValue("ship_line2"),
			City:       c.FormValue("ship_city"),
			State:      c.FormValue("ship_state"),
			PostalCode: c.FormValue("ship_postal_code"),
			Country:    c.FormValue("ship_country"),
		}.Trimmed(),
		FileNames:      formValuesTrimmed(c, "file_name"),
		ColorMaterials: formValuesTrimmed(c, "color_material"),
		Quantities:     formValuesTrimmed(c, "quantity_required"),
	}
}

// formValuesTrimmed returns every submitted value for key, in order, for
// both urlencoded bodies (PostArgs) and multipart bodies (the portal
// form, which also carries file uploads) - PostArgs is empty for
// multipart, so the two have to be read differently.
func formValuesTrimmed(c *fiber.Ctx, key string) []string {
	var raw []string
	if strings.HasPrefix(c.Get(fiber.HeaderContentType), fiber.MIMEMultipartForm) {
		if form, err := c.MultipartForm(); err == nil {
			raw = form.Value[key]
		}
	} else {
		for _, b := range c.Context().PostArgs().PeekMulti(key) {
			raw = append(raw, string(b))
		}
	}
	out := make([]string, len(raw))
	for i, v := range raw {
		out[i] = strings.TrimSpace(v)
	}
	return out
}

// items builds the non-blank item rows (a row with no file name is a
// leftover empty row, not a real item) into board.NewOrderItem, defaulting
// a missing/invalid quantity to 1.
func (f newOrderForm) items() []board.NewOrderItem {
	var items []board.NewOrderItem
	for i, name := range f.FileNames {
		if name == "" {
			continue
		}
		qty := 1
		if i < len(f.Quantities) {
			if q, err := strconv.Atoi(f.Quantities[i]); err == nil && q > 0 {
				qty = q
			}
		}
		material := ""
		if i < len(f.ColorMaterials) {
			material = f.ColorMaterials[i]
		}
		items = append(items, board.NewOrderItem{FileName: name, ColorMaterial: material, QuantityRequired: qty})
	}
	return items
}

// validate returns a user-facing message for the first problem it finds,
// or "" if the form is ready to submit.
func (f newOrderForm) validate() string {
	switch {
	case f.OrderNumber == "":
		return "Order number is required."
	case f.CustomerID == 0 && f.CustomerName == "":
		return "Pick a customer or enter a customer name."
	}
	return f.validateDateAndItems()
}

// validateDateAndItems is the part of validate shared with the client
// portal form, where the order number and customer are set server-side.
func (f newOrderForm) validateDateAndItems() string {
	if f.DueDate == "" {
		return "Ship-by date is required."
	}
	if _, err := time.Parse(dueDateLayout, f.DueDate); err != nil {
		return "Ship-by date must be a valid date."
	}
	if missing := f.ShipTo.MissingField(); missing != "" {
		return "Ship-to " + missing + " is required."
	}
	if len(f.items()) == 0 {
		return "Add at least one print item (a file name)."
	}
	return ""
}

// toNewOrder assumes validate() already passed - the date parse error is
// ignored here since a failed parse would have already stopped the request
// in handleCreate.
func (f newOrderForm) toNewOrder() board.NewOrder {
	due, _ := time.Parse(dueDateLayout, f.DueDate)
	return board.NewOrder{
		OrderNumber:  f.OrderNumber,
		CustomerID:   f.CustomerID,
		CustomerName: f.CustomerName,
		DueDate:      due,
		ShipTo:       f.ShipTo,
		Items:        f.items(),
	}
}

// toFormData re-renders what was typed (plus errMsg) rather than a blank
// form, so a validation error never costs the user their input. Always
// pads back up to minItemRows so there's at least one empty row left to
// type into, even if every submitted row was blank.
func (f newOrderForm) toFormData(errMsg string) views.NewOrderFormData {
	rows := make([]views.NewOrderItemRow, 0, len(f.FileNames))
	for i, name := range f.FileNames {
		row := views.NewOrderItemRow{FileName: name}
		if i < len(f.ColorMaterials) {
			row.ColorMaterial = f.ColorMaterials[i]
		}
		if i < len(f.Quantities) {
			row.Quantity = f.Quantities[i]
		}
		rows = append(rows, row)
	}
	for len(rows) < minItemRows {
		rows = append(rows, views.NewOrderItemRow{})
	}

	return views.NewOrderFormData{
		OrderNumber:  f.OrderNumber,
		CustomerID:   f.CustomerID,
		CustomerName: f.CustomerName,
		DueDate:      f.DueDate,
		ShipTo:       f.ShipTo,
		Items:        rows,
		Error:        errMsg,
	}
}

func blankItemRows(n int) []views.NewOrderItemRow {
	rows := make([]views.NewOrderItemRow, n)
	return rows
}
