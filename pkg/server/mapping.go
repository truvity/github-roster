package server

import (
	"strings"

	"github.com/gofiber/fiber/v3"
)

// formValue reads a trimmed form field. Cloned because fasthttp reuses the
// backing buffer after the handler returns. Shared by the surviving operator
// POST handlers (org staging, the App-manifest flow).
func formValue(c fiber.Ctx, key string) string {
	return strings.Clone(strings.TrimSpace(c.FormValue(key)))
}
