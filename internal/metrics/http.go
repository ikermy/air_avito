package metrics

import (
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"
)

func FiberWrap(route string, next fiber.Handler) fiber.Handler {
	return func(c fiber.Ctx) error {
		startedAt := time.Now()
		err := next(c)
		normalizedRoute := NormalizeRoute(route)
		duration := time.Since(startedAt)
		HTTPRequestDuration.WithLabelValues(c.Method(), normalizedRoute).Observe(duration.Seconds())
		HTTPSRequests.WithLabelValues(c.Method(), normalizedRoute, strconv.Itoa(c.Response().StatusCode())).Inc()
		return err
	}
}
