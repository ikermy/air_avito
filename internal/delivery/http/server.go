package http

import (
	metrics "air_avito/internal/metrics"
	"context"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/ikermy/air-logger/v2/pkg/logger"
)

// AvitoHandlers содержит обработчики Avito, предоставляемые приложением.
type AvitoHandlers struct {
	ExtractUID                                                                     fiber.Handler
	Status, AuthURL, Enable, Disable, Chats, Subscriptions, Subscribe, Unsubscribe fiber.Handler
	Available, AuthCallback, Webhook                                               fiber.Handler
}

// StartAvitoServer запускает HTTP-сервер Avito и ожидает завершения контекста.
func StartAvitoServer(ctx context.Context, h AvitoHandlers) error {
	app := fiber.New()
	logger.Info("Web server air_avito started")

	app.Use(func(c fiber.Ctx) error {
		origin := c.Get("Origin")
		if strings.Contains(origin, "localhost") {
			c.Set("Access-Control-Allow-Origin", origin)
		}
		c.Set("Access-Control-Allow-Credentials", "true")
		c.Set("Access-Control-Allow-Methods", "POST, GET, DELETE, OPTIONS")
		c.Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if c.Method() == "OPTIONS" {
			return c.SendStatus(fiber.StatusNoContent)
		}
		return c.Next()
	})
	app.Get("/metrics", metrics.Handler())
	app.Get("/avito/available", h.Available)
	v1 := app.Group("/v1")
	v1.Get("/avito/status", h.ExtractUID, metrics.FiberWrap("/avito/status", h.Status))
	v1.Post("/avito/auth/url", h.ExtractUID, metrics.FiberWrap("/avito/auth/url", h.AuthURL))
	v1.Get("/avito/enable", h.ExtractUID, metrics.FiberWrap("/avito/enable", h.Enable))
	v1.Get("/avito/disable", h.ExtractUID, metrics.FiberWrap("/avito/disable", h.Disable))
	v1.Get("/avito/chats", h.ExtractUID, metrics.FiberWrap("/avito/chats", h.Chats))
	v1.Get("/avito/subscriptions", h.ExtractUID, metrics.FiberWrap("/avito/subscriptions", h.Subscriptions))
	v1.Post("/avito/subscribe", h.ExtractUID, metrics.FiberWrap("/avito/subscribe", h.Subscribe))
	v1.Post("/avito/unsubscribe", h.ExtractUID, metrics.FiberWrap("/avito/unsubscribe", h.Unsubscribe))
	open := app.Group("/open")
	open.Get("/avito/auth/callback", metrics.FiberWrap("/avito/auth/callback", h.AuthCallback))
	open.Post("/avito/webhook", metrics.FiberWrap("/avito/webhook", h.Webhook))

	go func() {
		if err := app.Listen("0.0.0.0:8080", fiber.ListenConfig{DisableStartupMessage: true}); err != nil {
			logger.Error("Ошибка запуска Avito сервера: %v", err)
		}
	}()
	<-ctx.Done()
	logger.Info("Avito: получен сигнал завершения, останавливаю сервер...")
	return app.Shutdown()
}
