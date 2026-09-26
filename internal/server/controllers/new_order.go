package controllers

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"print-kds/internal/board"
	"print-kds/internal/logger"
	"print-kds/internal/views"

	"github.com/gofiber/fiber/v2"
	"github.com/valyala/fasthttp"
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

func (n *NewOrderController) RegisterRoutes(app *fiber.App) {
	app.Get("/admin/new-order", n.handleForm)
	app.Post("/admin/new-order", n.handleCreate)
	app.Get("/admin/new-order/item-row", n.handleItemRow)
}

func (n *NewOrderController) handleForm(c *fiber.Ctx) error {
	data := views.NewOrderFormData{
		Items:              blankItemRows(minItemRows),
		CreatedOrderNumber: c.Query("created"),
	}
	return render(c, views.NewOrderForm(data))
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

	if msg := form.validate(); msg != "" {
		return render(c, views.NewOrderForm(form.toFormData(msg)))
	}

	if _, err := n.store.CreateOrder(c.Context(), form.toNewOrder()); err != nil {
		if errors.Is(err, board.ErrDuplicateOrderNumber) {
			return render(c, views.NewOrderForm(form.toFormData(fmt.Sprintf("Order number %q already exists - pick a different one.", form.OrderNumber))))
		}
		logger.Error("create order", "error", err)
		return render(c, views.NewOrderForm(form.toFormData("Something went wrong saving this order - try again.")))
	}

	return c.Redirect(fmt.Sprintf("/admin/new-order?created=%s", form.OrderNumber), fiber.StatusSeeOther)
}

// newOrderForm holds one submission's raw values - item fields are
// parallel arrays (index i is one row) since the form repeats the same
// input names across every item row rather than indexing them.
type newOrderForm struct {
	OrderNumber    string
	CustomerName   string
	DueDate        string
	FileNames      []string
	ColorMaterials []string
	Quantities     []string
}

func parseNewOrderForm(c *fiber.Ctx) newOrderForm {
	args := c.Context().PostArgs()
	return newOrderForm{
		OrderNumber:    strings.TrimSpace(c.FormValue("order_number")),
		CustomerName:   strings.TrimSpace(c.FormValue("customer_name")),
		DueDate:        strings.TrimSpace(c.FormValue("due_date")),
		FileNames:      peekMultiTrimmed(args, "file_name"),
		ColorMaterials: peekMultiTrimmed(args, "color_material"),
		Quantities:     peekMultiTrimmed(args, "quantity_required"),
	}
}

func peekMultiTrimmed(args *fasthttp.Args, key string) []string {
	raw := args.PeekMulti(key)
	out := make([]string, len(raw))
	for i, b := range raw {
		out[i] = strings.TrimSpace(string(b))
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
	case f.CustomerName == "":
		return "Customer name is required."
	case f.DueDate == "":
		return "Due date is required."
	}
	if _, err := time.Parse(dueDateLayout, f.DueDate); err != nil {
		return "Due date must be a valid date."
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
		CustomerName: f.CustomerName,
		DueDate:      due,
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
		CustomerName: f.CustomerName,
		DueDate:      f.DueDate,
		Items:        rows,
		Error:        errMsg,
	}
}

func blankItemRows(n int) []views.NewOrderItemRow {
	rows := make([]views.NewOrderItemRow, n)
	return rows
}
