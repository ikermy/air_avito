package avito

import (
	domainavito "air_avito/internal/domain"
	"air_avito/internal/metrics"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/ikermy/air_common/pkg/mode"
	"github.com/ikermy/air_common/pkg/model"
	"github.com/ikermy/air_logger/v2/pkg/logger"
)

// StatusHandler проверяет статус подключения Avito для пользователя
// GET /avito/status?uid={userId}
func (u *User) StatusHandler(c fiber.Ctx) error {
	// Получаем userId из контекста (установлен middleware extractUID)
	userId := c.Locals("userId").(uint32)

	// Проверяем наличие токена в БД
	token, err := u.db.GetAvitoToken(u.ctx, userId, u.userMasterKey(userId))
	if err != nil {
		// Токен не найден - Avito не подключен
		return c.JSON(fiber.Map{
			"connected":     false,
			"avito_user_id": nil,
		})
	}

	// Токен найден - Avito подключен
	return c.JSON(fiber.Map{
		"connected":     true,
		"avito_user_id": token.AvitoUserID,
	})
}

// AuthURLHandler генерирует URL для OAuth авторизации
// POST /avito/auth/url?uid={userId}
// Body: {"url": "your-domain.ngrok-free.dev", "client_id": "...", "client_secret": "..."}
// Формат url может быть любым:
//   - "info-bot.online"
//   - "https://info-bot.online"
//   - "https://info-bot.online/"
//   - "https://info-bot.online/open/avito/auth/callback"
//
// Из него будет извлечён только домен, и автоматически будет сформирован redirect_uri:
//
//	https://{domain}/open/avito/auth/callback
func (u *User) AuthURLHandler(c fiber.Ctx) error {
	// Получаем userId из контекста (установлен middleware extractUID)
	userId := c.Locals("userId").(uint32)

	// Структура для парсинга JSON body
	type AuthURLRequest struct {
		URL          string `json:"url"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}

	var req AuthURLRequest
	if err := c.Bind().JSON(&req); err != nil {
		logger.Error("'AuthURLHandler' Ошибка парсинга JSON body: %v", err, userId)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "invalid JSON body",
		})
	}

	// Проверяем обязательные параметры
	if req.URL == "" || req.ClientID == "" || req.ClientSecret == "" {
		logger.Error("'AuthURLHandler' Отсутствуют обязательные параметры: url, client_id или client_secret", userId)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "url, client_id and client_secret are required in JSON body",
		})
	}

	// Нормализуем URL - извлекаем только домен
	normalizedDomain := normalizeRedirectURL(req.URL)
	logger.Debug("'AuthURLHandler' Нормализация URL: исходный='%s', нормализованный='%s'", req.URL, normalizedDomain, userId)

	// Формируем полный redirect_uri
	redirectURI := fmt.Sprintf("https://%s/open/avito/auth/callback", normalizedDomain)

	// Сохраняем настройки во временное хранилище
	u.pendingOAuthSettings.Store(userId, &domainavito.PendingOAuthSettings{
		ClientID:          req.ClientID,
		ClientSecret:      req.ClientSecret,
		RedirectURLPrefix: normalizedDomain, // Сохраняем только домен
		CreatedAt:         time.Now(),
	})

	authURL := u.GenerateAuthURL(userId, redirectURI, req.ClientID)

	logger.Debug("Сгенерирован OAuth URL для пользователя с redirect_uri: %s, client_id: %s", redirectURI, req.ClientID, userId)

	return c.JSON(fiber.Map{
		"auth_url": authURL,
	})
}

// AuthCallbackHandler обрабатывает OAuth callback от Avito
// GET /open/avito/auth/callback?code=...&state=...&error=...
func (u *User) AuthCallbackHandler(c fiber.Ctx) error {
	host := mode.GetRealHost() // NEED TEST

	code := c.Query("code")
	state := c.Query("state")
	errorParam := c.Query("error")

	// Проверка на ошибку от Avito
	if errorParam != "" {
		logger.Error("'AuthCallbackHandler' Ошибка OAuth от Avito: %s", errorParam)
		return c.Redirect().To(fmt.Sprintf("%s/auth/avito/error?reason=%s", host, errorParam))
	}

	if code == "" || state == "" {
		logger.Error("'AuthCallbackHandler' Отсутствует code или state")
		return c.Redirect().To(fmt.Sprintf("%s/auth/avito/error?reason=missing-params", host))
	}

	// Извлекаем userId из state
	uid, err := strconv.ParseUint(state, 10, 32)
	if err != nil {
		logger.Error("'AuthCallbackHandler' Ошибка парсинга state: %v", err)
		return c.Redirect().To(fmt.Sprintf("%s/auth/avito/error?reason=invalid-state", host))
	}

	userId := uint32(uid)

	// Извлекаем персональные настройки из временного хранилища
	var pendingSettings *domainavito.PendingOAuthSettings
	if settingsInterface, ok := u.pendingOAuthSettings.LoadAndDelete(userId); ok {
		pendingSettings = settingsInterface.(*domainavito.PendingOAuthSettings)
	} else {
		logger.Error("'AuthCallbackHandler' Персональные настройки не найдены", userId)
		return c.Redirect().To(fmt.Sprintf("%s/auth/avito/error?reason=settings-not-found", host))
	}

	// Обмениваем code на токены с персональными настройками
	if err := u.ExchangeCodeForToken(code, userId, u.db, pendingSettings); err != nil {
		logger.Error("'AuthCallbackHandler' Ошибка обмена code на токены: %v", err, userId)
		return c.Redirect().To(fmt.Sprintf("%s/auth/avito/error?reason=exchange-failed", host))
	}

	logger.Debug("'AuthCallbackHandler' Avito OAuth успешно завершен", userId)

	// Получаем Avito User ID для передачи на фронтенд
	token, err := u.db.GetAvitoToken(u.ctx, userId, u.userMasterKey(userId))
	avitoUserID := ""
	if err == nil && token != nil {
		avitoUserID = token.AvitoUserID
	}

	// Запускаем клиента для этого пользователя, если он еще не запущен
	if _, exists := u.clients.Load(userId); !exists {
		// Получаем данные пользователя из БД
		userDetails, err := u.db.GetAvitoUser(u.ctx, userId)
		if err != nil {
			logger.Error("'AuthCallbackHandler' Ошибка получения данных пользователя: %v", err, userId)
		} else if userDetails != nil {
			// Создаем модель ассистента
			assist := &model.Assistant{
				UserID:     userId,
				AssistName: userDetails.AssistName,
				AssistId:   userDetails.AssistantID,
				Provider:   userDetails.Provider,
				Metas: model.Target{
					MetaAction: userDetails.MetaAction,
					Triggers:   userDetails.Triggers,
				},
				Events: model.Notifications{
					Start:  userDetails.Events.Start,
					End:    userDetails.Events.End,
					Target: userDetails.Events.Target,
				},
				Limit:  userDetails.AskLimit,
				Espero: userDetails.Espero,
				Ignore: userDetails.Ignore,
			}

			// Создаем клиента
			client := NewClient(u, userId, assist)

			u.clients.Store(userId, client)

			metrics.SetActiveSessions(userId, "running", 1)
			metrics.TrackActiveDialogs(userId, 0)
			metrics.TrackOperatorModeDialogs(userId, 0)

			logger.Info("Avito клиент запущен для пользователя после OAuth", userId)
		}
	}

	// Автоматически подписываемся на webhook'и (ПОСЛЕ создания клиента!)
	if err := u.SubscribeToWebhooks(userId, u.db); err != nil {
		logger.Warn("'AuthCallbackHandler' Не удалось подписаться на webhook'и: %v (можно настроить позже)", err, userId)
		// Не прерываем процесс, webhook'и можно настроить позже вручную
	}

	// Редирект на страницу успеха с Avito User ID
	logger.Info("'AuthCallbackHandler' Успешно завершен OAuth, редирект на success страницу", userId)
	return c.Redirect().To(fmt.Sprintf("%s/auth/avito/success?avito_user_id=%s", host, avitoUserID))
}

// AvailableHandler проверяет доступность сервиса
func (u *User) AvailableHandler(c fiber.Ctx) error {
	return c.SendStatus(fiber.StatusOK)
}

// EnableHandler запускает Avito бота для пользователя
// GET /avito/enable?uid={userId}
func (u *User) EnableHandler(c fiber.Ctx) error {
	// Получаем userId из контекста (установлен middleware extractUID)
	userId := c.Locals("userId").(uint32)

	// Запускаем клиента
	err := u.StartUserClient(userId)
	if err != nil {
		logger.Error("'EnableHandler' Ошибка запуска Avito бота для пользователя: %v", err, userId)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to start avito bot",
		})
	}

	// Проверяем/подписываемся на webhook'и (на случай если не было подписки ранее)
	if err := u.SubscribeToWebhooks(userId, u.db); err != nil {
		logger.Warn("'EnableHandler' Не удалось подписаться на webhook'и: %v", err, userId)
	}

	logger.Info("'EnableHandler' Avito бот успешно запущен", userId)
	metrics.SetActiveSessions(userId, "running", 1)
	return c.JSON(fiber.Map{
		"status": "avito bot started successfully",
	})
}

// DisableHandler останавливает Avito бота для пользователя
// GET /avito/disable?uid={userId}
func (u *User) DisableHandler(c fiber.Ctx) error {
	// Получаем userId из контекста (установлен middleware extractUID)
	userId := c.Locals("userId").(uint32)

	// Останавливаем клиента
	err := u.StopUserClient(userId)
	if err != nil {
		logger.Error("'DisableHandler' Ошибка остановки Avito бота для пользователя: %v", err, userId)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to stop avito bot",
		})
	}

	logger.Info("'DisableHandler' Avito бот успешно остановлен", userId)
	metrics.SetActiveSessions(userId, "running", 0)
	metrics.TrackActiveDialogs(userId, 0)
	metrics.TrackOperatorModeDialogs(userId, 0)
	return c.JSON(fiber.Map{
		"status": "avito bot stopped successfully",
	})
}

// ChatsHandler получает список чатов для проверки авторизации
// GET /avito/chats?uid={userId}&limit={limit}&offset={offset}&unread_only={bool}
func (u *User) ChatsHandler(c fiber.Ctx) error {
	// Получаем userId из контекста (установлен middleware extractUID)
	userId := c.Locals("userId").(uint32)

	// Получаем query параметры (с значениями по умолчанию)
	limit := 10 // По умолчанию 10 чатов
	if limitStr := c.Query("limit"); limitStr != "" {
		if val, err := strconv.Atoi(limitStr); err == nil {
			limit = val
		}
	}

	offset := 0 // По умолчанию с начала
	if offsetStr := c.Query("offset"); offsetStr != "" {
		if val, err := strconv.Atoi(offsetStr); err == nil {
			offset = val
		}
	}

	unreadOnly := false // По умолчанию false
	if unreadStr := c.Query("unread_only"); unreadStr == "true" || unreadStr == "1" {
		unreadOnly = true
	}

	// Валидация параметров
	if limit < 1 || limit > 100 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "limit must be between 1 and 100",
		})
	}
	if offset < 0 || offset > 1000 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "offset must be between 0 and 1000",
		})
	}

	// Получаем чаты через API
	chatsResp, err := u.GetChats(userId, u.db, limit, offset, unreadOnly)
	if err != nil {
		logger.Error("'ChatsHandler' Ошибка получения чатов: %v", err, userId)

		// Проверяем тип ошибки для возврата правильного статуса
		errMsg := err.Error()
		if strings.Contains(errMsg, "токена из БД") {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"error": "avito not connected",
			})
		}

		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to fetch chats",
		})
	}

	logger.Debug("'ChatsHandler' Успешно получено чатов: %d", len(chatsResp.Chats), userId)
	return c.JSON(chatsResp)
}

// SubscriptionsHandler получает список webhook подписок
// GET /avito/subscriptions?uid={userId}
func (u *User) SubscriptionsHandler(c fiber.Ctx) error {
	// Получаем userId из контекста (установлен middleware extractUID)
	userId := c.Locals("userId").(uint32)

	// Получаем подписки через API
	subsResp, err := u.GetSubscriptions(userId)
	if err != nil {
		logger.Error("'SubscriptionsHandler' Ошибка получения подписок: %v", err, userId)

		// Проверяем тип ошибки для возврата правильного статуса
		errMsg := err.Error()
		if strings.Contains(errMsg, "токена из БД") {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"error": "avito not connected",
			})
		}

		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to fetch subscriptions",
		})
	}

	logger.Debug("'SubscriptionsHandler' Успешно получено подписок: %d", len(subsResp.Subscriptions), userId)
	return c.JSON(subsResp)
}

// SubscribeHandler подписывается на webhook'и
// POST /avito/subscribe?uid={userId}
func (u *User) SubscribeHandler(c fiber.Ctx) error {
	// Получаем userId из контекста (установлен middleware extractUID)
	userId := c.Locals("userId").(uint32)

	// Подписываемся на webhook'и
	if err := u.SubscribeToWebhooks(userId, u.db); err != nil {
		logger.Error("'SubscribeHandler' Ошибка подписки на webhook'и: %v", err, userId)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to subscribe to webhooks",
		})
	}

	// Получаем токен для возврата правильного URL
	token, _ := u.db.GetAvitoToken(u.ctx, userId, u.userMasterKey(userId))
	webhookURL := ""
	if token != nil && token.RedirectURLPrefix != "" {
		// Нормализуем URL на случай старых записей в БД
		normalizedDomain := normalizeRedirectURL(token.RedirectURLPrefix)
		webhookURL = fmt.Sprintf("https://%s/open/avito/webhook", normalizedDomain)
	} else {
		// mode.RealHost уже содержит протокол https://
		webhookURL = fmt.Sprintf("%s/open/avito/webhook", mode.GetRealHost())
	}

	logger.Info("'SubscribeHandler' Успешная подписка на webhook'и", userId)
	return c.JSON(fiber.Map{
		"status": "subscribed successfully",
		"url":    webhookURL,
	})
}

// UnsubscribeHandler отписывается от webhook'а
// POST /avito/unsubscribe?uid={userId}&url={webhookURL}
func (u *User) UnsubscribeHandler(c fiber.Ctx) error {
	// Получаем userId из контекста (установлен middleware extractUID)
	userId := c.Locals("userId").(uint32)

	// Получаем URL для отписки
	webhookURL := c.Query("url")
	if webhookURL == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "url parameter is required",
		})
	}

	// Отписываемся от webhook'а
	if err := u.UnsubscribeFromWebhooks(userId, webhookURL); err != nil {
		logger.Error("'UnsubscribeHandler' Ошибка отписки от webhook'а: %v", err, userId)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to unsubscribe from webhook",
		})
	}

	logger.Info("'UnsubscribeHandler' Успешная отписка от webhook'а", userId)
	return c.JSON(fiber.Map{
		"status": "unsubscribed successfully",
		"url":    webhookURL,
	})
}
