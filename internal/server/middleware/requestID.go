package middleware

import (
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/requestid"
	"github.com/gofiber/fiber/v2/utils"
)

// requestIDContextKey is where CurrentRequestID (below) reads back what
// requestid.New() stashed in locals - matches that middleware's own
// ConfigDefault.ContextKey rather than inventing a second name for the
// same value.
const requestIDContextKey = "requestid"

// RequestID wraps fiber's own requestid middleware: one id per request,
// echoed back as the X-Request-ID response header. Register it first,
// ahead of everything else (including Recover) - Recover's own log line
// reads it back.
func RequestID() fiber.Handler {
	return requestid.New(requestid.Config{
		ContextKey: requestIDContextKey,
		Generator:  utils.UUIDv4,
	})
}

// CurrentRequestID reads the id RequestID generated for this request.
func CurrentRequestID(c *fiber.Ctx) string {
	id, _ := c.Locals(requestIDContextKey).(string)
	return id
}
