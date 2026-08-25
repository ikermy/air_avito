package avito

import (
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/ikermy/air-common/pkg/com"
	"github.com/ikermy/air-logger/v2/pkg/logger"
)

// extractUID middleware извлекает userID из query параметра uid и сохраняет в Locals
// Landing проксирует запросы и передаёт uid из токена авторизации
func (u *User) extractUID(c fiber.Ctx) error {
	// Получаем uid из query параметра (Landing передаёт его после извлечения из токена)
	uidStr := c.Query("uid")
	if uidStr == "" {
		logger.Warn("Отсутствует параметр uid в запросе")
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "uid parameter required",
		})
	}

	// Парсим uid в uint32
	uid, err := strconv.ParseUint(uidStr, 10, 32)
	if err != nil {
		logger.Error("Некорректный формат uid: %v", err)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "invalid uid format",
		})
	}

	userId := uint32(uid)

	// Сохраняем userId в контексте для использования в handlers
	c.Locals("userId", userId)

	return c.Next()
}

// userMasterKey Получает MasterKey пользователя от Landing
func (u *User) userMasterKey(userID uint32) [32]byte {
	var (
		userMasterKey [32]byte
		err           error
	)

	if u.rpc != nil {
		if userMasterKey, err = u.rpc.GetUserMasterKey(u.ctx, userID); err == nil {
			// ключ успешно получен
			return userMasterKey
		}

		// ошибка получения ключа
		logger.Warn("Ошибка получения MasterKey: %v (требуется вход на Landing)", err, userID)

		expireAt, ok := u.needReauthorization[userID]
		if ok && time.Now().Before(expireAt) {
			// ещё не прошло 60 минут — не отправляем уведомление повторно
			return [32]byte{}
		}

		notifyMsg := com.CarpCh{
			Event:  "reauth-userkey",
			UserID: userID,
		}
		if err := u.end.SendNotification(notifyMsg); err != nil {
			logger.Error("Ошибка отправки уведомления о повторной аутентификации %v", err, userID)
		} else {
			// обновляем TTL на 60 минут
			u.needReauthorization[userID] = time.Now().Add(60 * time.Minute)
		}
	}

	// если orc == nil или ключ не получен
	return [32]byte{}
}

func (c *Client) sendFirstContactMessage(senderID uint64, respName string) {
	// Ищу пользователя в кеше редис
	isFirstInteraction := true
	if c.redisCache != nil {
		exists, err := c.redisCache.Has(c.ctx, c.userID, int64(senderID))
		if err != nil {
			logger.Warn("Redis: ошибка проверки firstInteraction для senderID=%d: %v", senderID, err, c.userID)
		}
		logger.Debug("Redis: проверка firstInteraction для senderID=%d, exists=%v", senderID, exists, c.userID)
		isFirstInteraction = !exists
	}

	// Отправляем уведомление о начале диалога, если требуется
	if isFirstInteraction && c.assist.Events.Start {
		msg := com.CarpCh{
			Event:      "start",
			UserName:   respName,
			AssistName: c.assist.AssistName,
			Target:     "",
			UserID:     c.userID,
		}
		err := c.end.SendNotification(msg)
		if err != nil {
			logger.Error("Ошибка отправки уведомления о начале диалога: %v", err, c.userID)
		}
	}

	// Сохраняю пользователя в редис после отправки уведомления
	if c.redisCache != nil {
		err := c.redisCache.Set(c.ctx, c.userID, int64(senderID))
		if err != nil {
			logger.Warn("Redis: ошибка сохранения firstInteraction для senderID=%d: %v", senderID, err, c.userID)
		}
	}
}

func (c *Client) preloadFirstInteraction() {
	if c.redisCache == nil {
		return
	}

	senderIDs, err := c.redisCache.LoadUser(c.ctx, c.userID)
	if err != nil {
		logger.Warn("Redis: не удалось прогреть knownUsers: %v", err, c.userID)
		return
	}

	for _, senderID := range senderIDs {
		c.knownUsers.Store(senderID, true)
	}

	if len(senderIDs) > 0 {
		logger.Debug("Redis: прогрето knownUsers=%d", len(senderIDs), c.userID)
	}
}
