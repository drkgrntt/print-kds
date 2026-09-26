package controllers

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"print-kds/internal/board"
	"print-kds/internal/logger"
	"print-kds/internal/server/middleware"
	"print-kds/internal/views"
	"print-kds/internal/views/components"

	"github.com/gofiber/fiber/v2"
)

type BoardController struct {
	store *board.Store
}

func NewBoardController(store *board.Store) *BoardController {
	return &BoardController{store: store}
}

func (b *BoardController) RegisterRoutes(app *fiber.App) {
	app.Get("/", b.handleBoard)
	app.Post("/print-items/:id/start", b.handleStartPrint)
	app.Post("/print-items/:id/pop", b.handleTransition(b.store.PopOffBed))
	app.Post("/print-items/:id/fail-print", b.handleFailPrint)
	app.Post("/print-items/:id/qc-result", b.handleQCResult)
	app.Post("/print-items/:id/generate-label", b.handleGenerateLabel)
	app.Post("/print-items/:id/cancel", b.handleTransition(b.store.CancelItem))
	app.Post("/print-items/:id/quantity", b.handleUpdateQuantity)
	app.Get("/print-items/:id/history", b.handleItemHistory)
}

func (b *BoardController) handleBoard(c *fiber.Ctx) error {
	tenantID := middleware.CurrentTenantID(c)
	columns, err := b.store.BoardColumns(c.Context(), tenantID)
	if err != nil {
		logger.Error("load board columns", "error", err)
		return c.Status(fiber.StatusInternalServerError).SendString("failed to load board")
	}
	layout := views.LayoutData{TenantName: middleware.CurrentTenantName(c), IsAdmin: middleware.IsAdmin(c)}
	return render(c, views.Board(columns, layout))
}

// transitionFunc matches every board.Store status-transition method except
// GenerateLabel (which also returns a *Shipment - see handleGenerateLabel).
type transitionFunc func(ctx context.Context, tenantID, userID, itemID int64) (board.PrintItem, error)

// handleTransition builds one htmx endpoint handler around a
// board.Store transition method: parse :id, run the transition, then write
// the delete+insert OOB swap pair (see writeOOBSwap). POP OFF BED and
// CANCEL are the only board actions that fit this shape unmodified - START
// PRINT (assigned_printer), PRINT FAILED (reason), PASS QC/RE-PRINT (extra
// form fields and a possible split return value), GENERATE LABEL (an extra
// *Shipment return value), and quantity edit (extra new_quantity field)
// each need their own handler below instead.
func (b *BoardController) handleTransition(fn transitionFunc) fiber.Handler {
	return func(c *fiber.Ctx) error {
		itemID, err := parseItemID(c)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).SendString(err.Error())
		}

		item, err := fn(c.Context(), middleware.CurrentTenantID(c), middleware.CurrentUserID(c), itemID)
		if err != nil {
			return respondTransitionError(c, err)
		}

		return writeOOBSwap(c, item)
	}
}

// handleStartPrint isn't a transitionFunc (StartPrint takes an extra
// assigned_printer form field) so it gets its own handler rather than
// going through handleTransition. assigned_printer is free text and
// optional - an empty value is passed through unvalidated (see the
// "queued" case in components.cardActions for the mini-form that posts
// here).
func (b *BoardController) handleStartPrint(c *fiber.Ctx) error {
	itemID, err := parseItemID(c)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).SendString(err.Error())
	}

	assignedPrinter := c.FormValue("assigned_printer")

	item, err := b.store.StartPrint(c.Context(), middleware.CurrentTenantID(c), middleware.CurrentUserID(c), itemID, assignedPrinter)
	if err != nil {
		return respondTransitionError(c, err)
	}

	return writeOOBSwap(c, item)
}

// handleFailPrint isn't a transitionFunc (FailPrint takes an extra reason
// form field) so it gets its own handler rather than going through
// handleTransition. This backs the "printing" column's PRINT FAILED
// action, which sends the item straight back to queued instead of routing
// it through cleaning/QC first - see board.Store.FailPrint.
func (b *BoardController) handleFailPrint(c *fiber.Ctx) error {
	itemID, err := parseItemID(c)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).SendString(err.Error())
	}

	reason := c.FormValue("reason")

	item, err := b.store.FailPrint(c.Context(), middleware.CurrentTenantID(c), middleware.CurrentUserID(c), itemID, reason)
	if err != nil {
		return respondTransitionError(c, err)
	}

	return writeOOBSwap(c, item)
}

// handleGenerateLabel isn't a transitionFunc (GenerateLabel also returns a
// *Shipment) so it gets its own handler rather than going through
// handleTransition. The shipment itself isn't surfaced on the board yet -
// there's no tracking/label UI in this pass - so it's discarded here; a
// future label/packing-slip view would read it from here instead of
// re-querying.
func (b *BoardController) handleGenerateLabel(c *fiber.Ctx) error {
	itemID, err := parseItemID(c)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).SendString(err.Error())
	}

	item, _, err := b.store.GenerateLabel(c.Context(), middleware.CurrentTenantID(c), middleware.CurrentUserID(c), itemID)
	if err != nil {
		return respondTransitionError(c, err)
	}

	return writeOOBSwap(c, item)
}

// handleQCResult isn't a transitionFunc (RecordQCResult takes extra form
// fields and can return a second, brand-new split item) so it gets its own
// handler rather than going through handleTransition. quantity_passed and
// failure_reason come from the posted form - see the cleaning_qc case in
// components.cardActions for the three shapes that hit this endpoint (full
// pass, full reprint, and the partial-QC mini-form).
func (b *BoardController) handleQCResult(c *fiber.Ctx) error {
	itemID, err := parseItemID(c)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).SendString(err.Error())
	}

	quantityPassed, err := strconv.Atoi(c.FormValue("quantity_passed"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).SendString("invalid quantity_passed")
	}
	failureReason := c.FormValue("failure_reason")

	item, split, err := b.store.RecordQCResult(c.Context(), middleware.CurrentTenantID(c), middleware.CurrentUserID(c), itemID, quantityPassed, failureReason)
	if err != nil {
		if errors.Is(err, board.ErrInvalidQuantity) {
			return c.Status(fiber.StatusBadRequest).SendString(err.Error())
		}
		return respondTransitionError(c, err)
	}

	c.Set("Content-Type", "text/html; charset=utf-8")
	if err := components.DeleteOOB(item.ID).Render(c.Context(), c); err != nil {
		return err
	}
	if err := components.InsertOOB(item.Status, item).Render(c.Context(), c); err != nil {
		return err
	}
	if split != nil {
		if err := components.InsertOOB(split.Status, *split).Render(c.Context(), c); err != nil {
			return err
		}
	}
	return nil
}

// handleUpdateQuantity isn't a transitionFunc (UpdateQuantity takes an extra
// new_quantity form field) so it gets its own handler rather than going
// through handleTransition. Backs the "queued" column's inline
// quantity-edit control (see components.cardActions) - a customer reducing
// or increasing an order's quantity before printing has started.
func (b *BoardController) handleUpdateQuantity(c *fiber.Ctx) error {
	itemID, err := parseItemID(c)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).SendString(err.Error())
	}

	newQuantity, err := strconv.Atoi(c.FormValue("new_quantity"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).SendString("invalid new_quantity")
	}

	item, err := b.store.UpdateQuantity(c.Context(), middleware.CurrentTenantID(c), middleware.CurrentUserID(c), itemID, newQuantity)
	if err != nil {
		if errors.Is(err, board.ErrInvalidQuantity) {
			return c.Status(fiber.StatusBadRequest).SendString(err.Error())
		}
		return respondTransitionError(c, err)
	}

	return writeOOBSwap(c, item)
}

// handleItemHistory backs the card's [ HISTORY ] toggle: a plain htmx GET
// swapped into a target div inside the card (hx-swap="innerHTML"), not an
// OOB swap - it's the first read-only htmx fragment in the app, since it
// doesn't move the card between columns the way every other endpoint here
// does. No RequireAdmin: any logged-in user can view an item's history,
// same as the board itself.
func (b *BoardController) handleItemHistory(c *fiber.Ctx) error {
	itemID, err := parseItemID(c)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).SendString(err.Error())
	}

	events, err := b.store.ItemHistory(c.Context(), middleware.CurrentTenantID(c), itemID)
	if err != nil {
		logger.Error("load item history", "error", err)
		return c.Status(fiber.StatusInternalServerError).SendString("failed to load history")
	}

	return render(c, components.HistoryList(events))
}

// writeOOBSwap writes the delete-old-card + insert-new-card OOB fragment
// pair that moves item's card to its new column client-side. A packed or
// cancelled item drops off the live board entirely (see
// board.Store.BoardColumns), so there's nothing to insert for either case -
// just the delete.
func writeOOBSwap(c *fiber.Ctx, item board.PrintItem) error {
	c.Set("Content-Type", "text/html; charset=utf-8")
	if err := components.DeleteOOB(item.ID).Render(c.Context(), c); err != nil {
		return err
	}
	if item.Status == "packed" || item.Status == "cancelled" {
		return nil
	}
	return components.InsertOOB(item.Status, item).Render(c.Context(), c)
}

// respondTransitionError turns a board.Store transition error into an HTTP
// response: ErrNotFound (no such item) is a 404, anything else (wrong
// starting status, a DB error) is a 409 - the board UI only ever offers the
// one button valid for an item's current status, so a 409 here means
// another tap already moved it (two tablets racing the same card) rather
// than a client bug.
func respondTransitionError(c *fiber.Ctx, err error) error {
	logger.Error("print item transition failed", "error", err)
	if errors.Is(err, board.ErrNotFound) {
		return c.Status(fiber.StatusNotFound).SendString(err.Error())
	}
	return c.Status(fiber.StatusConflict).SendString(err.Error())
}

func parseItemID(c *fiber.Ctx) (int64, error) {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid print item id")
	}
	return id, nil
}
