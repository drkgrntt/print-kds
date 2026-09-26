package middleware

import (
	"runtime/debug"

	"print-kds/internal/logger"

	"github.com/gofiber/fiber/v2"
)

// Recover catches a panic anywhere downstream and turns it into a logged
// 500 instead of taking down the whole process. Register this right after
// RequestID (routes.go) so the log line below can still tag the request
// that panicked.
func Recover() fiber.Handler {
	return func(c *fiber.Ctx) (err error) {
		defer func() {
			r := recover()
			if r == nil {
				return
			}

			logger.Error("panic recovered",
				"request_id", CurrentRequestID(c),
				"method", c.Method(),
				"path", c.Path(),
				"panic", r,
				"stack", string(debug.Stack()),
			)

			err = c.Status(fiber.StatusInternalServerError).SendString("Internal Server Error")
		}()

		return c.Next()
	}
}
