package controllers

import (
	"github.com/a-h/templ"
	"github.com/gofiber/fiber/v2"
)

// render writes a templ component as the response body. *fiber.Ctx
// satisfies io.Writer, so templ can render directly to it.
func render(c *fiber.Ctx, component templ.Component) error {
	c.Set("Content-Type", "text/html; charset=utf-8")
	return component.Render(c.Context(), c)
}
