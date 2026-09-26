package controllers

import (
	"print-kds/internal/database"

	"github.com/gofiber/fiber/v2"
)

type HealthController struct {
	db database.Service
}

func NewHealthController(db database.Service) *HealthController {
	return &HealthController{db: db}
}

func (h *HealthController) RegisterRoutes(app *fiber.App) {
	app.Get("/health", h.handleHealth)
}

func (h *HealthController) handleHealth(c *fiber.Ctx) error {
	return c.JSON(h.db.Health(c.Context()))
}
