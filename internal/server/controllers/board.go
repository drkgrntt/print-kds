package controllers

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"print-kds/internal/board"
	"print-kds/internal/logger"
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
	app.Post("/print-items/:id/start", b.handleTransition(b.store.StartPrint))
	app.Post("/print-items/:id/pop", b.handleTransition(b.store.PopOffBed))
	app.Post("/print-items/:id/pass-qc", b.handleTransition(b.store.PassQC))
	app.Post("/print-items/:id/reprint", b.handleTransition(b.store.RePrint))
	app.Post("/print-items/:id/generate-label", b.handleGenerateLabel)
}

func (b *BoardController) handleBoard(c *fiber.Ctx) error {
	columns, err := b.store.BoardColumns(c.Context())
	if err != nil {
		logger.Error("load board columns", "error", err)
		return c.Status(fiber.StatusInternalServerError).SendString("failed to load board")
	}
	return render(c, views.Board(columns))
}

// transitionFunc matches every board.Store status-transition method except
// GenerateLabel (which also returns a *Shipment - see handleGenerateLabel).
type transitionFunc func(ctx context.Context, itemID int64) (board.PrintItem, error)

// handleTransition builds one htmx endpoint handler around a
// board.Store transition method: parse :id, run the transition, then write
// the delete+insert OOB swap pair (see writeOOBSwap). START PRINT, POP OFF
// BED, PASS QC, and RE-PRINT all share this exact shape.
func (b *BoardController) handleTransition(fn transitionFunc) fiber.Handler {
	return func(c *fiber.Ctx) error {
		itemID, err := parseItemID(c)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).SendString(err.Error())
		}

		item, err := fn(c.Context(), itemID)
		if err != nil {
			return respondTransitionError(c, err)
		}

		return writeOOBSwap(c, item)
	}
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

	item, _, err := b.store.GenerateLabel(c.Context(), itemID)
	if err != nil {
		return respondTransitionError(c, err)
	}

	return writeOOBSwap(c, item)
}

// writeOOBSwap writes the delete-old-card + insert-new-card OOB fragment
// pair that moves item's card to its new column client-side. A packed item
// drops off the live board entirely (see board.Store.BoardColumns), so
// there's nothing to insert for that case - just the delete.
func writeOOBSwap(c *fiber.Ctx, item board.PrintItem) error {
	c.Set("Content-Type", "text/html; charset=utf-8")
	if err := components.DeleteOOB(item.ID).Render(c.Context(), c); err != nil {
		return err
	}
	if item.Status == "packed" {
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
