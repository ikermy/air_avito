package avito

import (
	"air_avito/internal/domain"
	"air_avito/internal/metrics"
	"encoding/json"

	"github.com/gofiber/fiber/v3"
	"github.com/ikermy/air_logger/v2/pkg/logger"
)

// WebhookHandler обрабатывает входящие webhook от Avito
func (u *User) WebhookHandler(c fiber.Ctx) error {
	metrics.IncMessagesReceived(0, "webhook")
	// ЛОГИРУЕМ КАЖДЫЙ ВХОДЯЩИЙ ЗАПРОС
	logger.Info("🔔 === WEBHOOK ВЫЗВАН === IP: %s, Method: %s, Path: %s", c.IP(), c.Method(), c.Path())

	// Читаем тело запроса
	body := c.Body()

	// ЛОГИРУЕМ RAW BODY
	logger.Info("📥 Webhook body (length=%d): %s", len(body), string(body))

	// Парсим payload
	var payload domain.WebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		metrics.IncMessagesIgnored(0, "invalid_payload")
		logger.Error("❌ Ошибка парсинга webhook payload: %v, body: %s", err, string(body))
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "invalid payload",
		})
	}

	// Логируем тип события
	logger.Info("✅ Получен Avito webhook: type=%s, chat_id=%s, message_text=%s",
		payload.Type, payload.Payload.ChatID, payload.Payload.Message.GetText())

	// Обрабатываем только входящие сообщения
	if payload.Type != "message" {
		metrics.IncMessagesIgnored(0, "unsupported_type")
		logger.Debug("⏭️ Пропускаем webhook type=%s (обрабатываем только 'message')", payload.Type)
		return c.SendStatus(fiber.StatusOK)
	}

	// Находим клиента по chat_id (нужно извлечь userId из БД или кеша)
	// Для упрощения пока обрабатываем через все клиенты
	clientsCount := 0
	u.clients.Range(func(key, value interface{}) bool {
		clientsCount++
		client := value.(*Client)
		logger.Debug("📤 Отправляем webhook сообщение клиенту userId=%d", client.userID)
		go func() {
			if err := client.handleIncomingMessage(payload); err != nil {
				logger.Error("Ошибка обработки сообщения: %v", err, client.userID)
			} else {
				// Помечаем чат как прочитанный после успешной обработки
				if err := client.MarkChatAsRead(payload.Payload.ChatID); err != nil {
					logger.Warn("Не удалось пометить чат как прочитанный: %v", err, client.userID)
				}
			}
		}()
		return true
	})

	logger.Info("✅ Webhook обработан успешно, отправлено %d клиентам", clientsCount)
	metrics.IncMessagesProcessed(0, "accepted")
	return c.SendStatus(fiber.StatusOK)
}
