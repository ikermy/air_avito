package avito

import (
	"air_avito/internal/domain"
	"air_avito/internal/metrics"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"path"
	"sync"
	"time"

	"github.com/ikermy/air-common/pkg/comdom"
	"github.com/ikermy/air-common/pkg/model"
	"github.com/ikermy/air-logger/v2/pkg/logger"
	"github.com/redis/go-redis/v9"
)

var StartCh = make(chan model.StartCh, 100) // Канал для запуска горутины слушателя

// New создает новый экземпляр User для управления Avito клиентами
func New(parent context.Context, d DB, m Model, e Endpoint, c CRM, o ORCClient, redisClient redis.UniversalClient) *User {
	ctx, cancel := context.WithCancel(parent)
	return &User{
		ctx:                 ctx,
		cancel:              cancel,
		db:                  d,
		mod:                 m,
		end:                 e,
		crm:                 c,
		rpc:                 o,
		redisCache:          newRedisFirstInteractionCache(redisClient),
		needReauthorization: make(map[uint32]time.Time),
	}
}

// NewClient создает нового клиента Avito для пользователя
func NewClient(u *User, userId uint32, assist *model.Assistant) *Client {
	ctx, cancel := context.WithCancel(u.ctx)

	// Инициализируем CRM для этого пользователя
	crmUser, debug, err := u.crm.Init(userId)
	if err != nil {
		// Может быть не ошибка, просто не настроена или отключена CRM
		logger.Debug("Ошибка инициализации CRM: %v", err, userId)
	}

	if debug != "" {
		logger.Debug("User инициализирован с настройками: %s", debug, userId)
	}

	// Загружаем персональные настройки из токена пользователя
	redirectUrlPrefix := ""

	var clientID, clientSecret string
	// Пытаемся получить токен из БД для извлечения персональных настроек
	token, err := u.db.GetAvitoToken(u.ctx, userId, u.userMasterKey(userId))
	if err == nil && token != nil {
		// Используем персональные настройки если они есть
		if token.ClientID != "" {
			clientID = token.ClientID
		}
		if token.ClientSecret != "" {
			clientSecret = token.ClientSecret
		}
		if token.RedirectURLPrefix != "" {
			// Нормализуем URL на случай старых записей в БД
			redirectUrlPrefix = normalizeRedirectURL(token.RedirectURLPrefix)
		}
	}

	client := &Client{
		ctx:               ctx,
		clientID:          clientID,
		clientSecret:      clientSecret,
		redirectUrlPrefix: redirectUrlPrefix,
		cancel:            cancel,
		userID:            userId,
		mk:                u.userMasterKey(userId),
		db:                u.db,
		mod:               u.mod,
		end:               u.end,
		crm:               crmUser,
		assist:            assist,
		redisCache:        u.redisCache,
	}

	// Гружу кеш первого взаимодействия из Redis
	go client.preloadFirstInteraction()

	// Запускаем очистку устаревших респондентов
	go client.cleanupStaleRespondents()

	// Запускаем периодический polling непрочитанных чатов
	go client.pollUnreadChats()

	// Запускаем начальную проверку непрочитанных чатов
	go client.checkInitialUnreadChats()
	metrics.SetActiveSessions(userId, "running", 1)
	metrics.TrackActiveDialogs(userId, 0)
	metrics.TrackOperatorModeDialogs(userId, 0)

	return client
}

// convertChatIDToDialogID конвертирует Avito chat_id в dialogId используя FNV-1a hash
func convertChatIDToDialogID(chatID string) (uint64, error) {
	h := fnv.New64a()
	if _, err := h.Write([]byte(chatID)); err != nil {
		return 0, fmt.Errorf("ошибка записи в хэш: %w", err)
	}
	return h.Sum64(), nil
}

// isImageType проверяет, является ли тип файла изображением
func isImageType(fileType model.FileType) bool {
	return fileType == model.Photo
}

// SendTextMessage отправляет текстовое сообщение в чат Avito
func (c *Client) SendTextMessage(chatID, text string) error {
	startedAt := time.Now()

	// Получаем токен из БД (включая AvitoUserID)
	token, err := c.db.GetAvitoToken(c.ctx, c.userID, c.mk)
	if err != nil {
		metrics.ObserveAppSend(c.userID, "token_error", startedAt)
		logger.Error("Ошибка получения Avito токена: %v", err, c.userID)
		return fmt.Errorf("не удалось получить токен: %w", err)
	}

	url := fmt.Sprintf("%s/accounts/%s/chats/%s/messages", domain.AvitoMessengerV3, token.AvitoUserID, chatID)

	reqBody := domain.SendMessageRequest{
		Message: domain.SendMessage{
			Text: text,
		},
	}

	resp, err := c.SendAPIRequest(http.MethodPost, url, reqBody)
	if err != nil {
		metrics.ObserveAppSend(c.userID, "request_error", startedAt)
		logger.Error("Ошибка отправки текстового сообщения в Avito: %v", err, c.userID)
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		metrics.ObserveAppSend(c.userID, "api_error", startedAt)
		body, _ := io.ReadAll(resp.Body)
		logger.Error("Avito API вернул ошибку: %s", string(body), c.userID)
		return fmt.Errorf("avito api error: status %d", resp.StatusCode)
	}

	logger.Debug("Текстовое сообщение успешно отправлено в Avito chat %s", chatID, c.userID)
	metrics.ObserveAppSend(c.userID, "success", startedAt)
	return nil
}

// SendImageMessage отправляет изображение по URL в чат Avito
func (c *Client) SendImageMessage(chatID, imageURL string) error {
	startedAt := time.Now()
	// Получаем токен из БД (включая AvitoUserID)
	token, err := c.db.GetAvitoToken(c.ctx, c.userID, c.mk)
	if err != nil {
		metrics.ObserveAppSend(c.userID, "token_error", startedAt)
		logger.Error("Ошибка получения Avito токена: %v", err, c.userID)
		return fmt.Errorf("не удалось получить токен: %w", err)
	}

	url := fmt.Sprintf("%s/accounts/%s/chats/%s/messages", domain.AvitoMessengerV3, token.AvitoUserID, chatID)

	reqBody := domain.SendMessageRequest{
		Message: domain.SendMessage{
			AttachmentType: "image",
			URL:            imageURL,
		},
	}

	resp, err := c.SendAPIRequest(http.MethodPost, url, reqBody)
	if err != nil {
		metrics.ObserveAppSend(c.userID, "request_error", startedAt)
		logger.Error("Ошибка отправки изображения в Avito: %v", err, c.userID)
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		metrics.ObserveAppSend(c.userID, "api_error", startedAt)
		body, _ := io.ReadAll(resp.Body)
		logger.Error("Avito API вернул ошибку при отправке изображения: %s", string(body), c.userID)
		return fmt.Errorf("avito api error: status %d", resp.StatusCode)
	}

	logger.Debug("Изображение успешно отправлено в Avito chat %s", chatID, c.userID)
	metrics.ObserveAppSend(c.userID, "success", startedAt)
	return nil
}

// GetChats получает список чатов пользователя (с возможностью фильтрации непрочитанных)
func (c *Client) GetChats(unreadOnly bool) ([]domain.Chat, error) {
	// Получаем валидный токен и AvitoUserID одновременно (обновляется автоматически если истек)
	accessToken, avitoUserID, err := c.GetValidTokenWithUserID()
	if err != nil {
		logger.Error("Ошибка получения валидного токена для GetChats: %v", err, c.userID)
		return nil, fmt.Errorf("не удалось получить токен: %w", err)
	}

	// Формируем URL с параметрами
	url := fmt.Sprintf("%s/accounts/%s/chats?limit=100", domain.AvitoMessengerV2, avitoUserID)
	if unreadOnly {
		url += "&unread_only=true"
	}

	// Отправляем запрос напрямую с уже полученным токеном
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("ошибка создания запроса: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		logger.Error("Ошибка запроса списка чатов Avito: %v", err, c.userID)
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		logger.Error("Avito API вернул ошибку при получении чатов: %s", string(body), c.userID)
		return nil, fmt.Errorf("avito api error: status %d", resp.StatusCode)
	}

	var chatsResp domain.ChatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatsResp); err != nil {
		logger.Error("Ошибка декодирования ответа чатов Avito: %v", err, c.userID)
		return nil, fmt.Errorf("не удалось декодировать ответ: %w", err)
	}

	return chatsResp.Chats, nil
}

// GetChatMessages получает список сообщений из конкретного чата
func (c *Client) GetChatMessages(chatID string, limit int) (domain.MessagesResponse, error) {
	// Получаем валидный токен и AvitoUserID одновременно (обновляется автоматически если истек)
	accessToken, avitoUserID, err := c.GetValidTokenWithUserID()
	if err != nil {
		logger.Error("Ошибка получения валидного токена для GetChatMessages: %v", err, c.userID)
		return nil, fmt.Errorf("не удалось получить токен: %w", err)
	}

	// Формируем URL с параметрами
	url := fmt.Sprintf("%s/accounts/%s/chats/%s/messages/?limit=%d",
		domain.AvitoMessengerV3, avitoUserID, chatID, limit)

	// Отправляем запрос напрямую с уже полученным токеном
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("ошибка создания запроса: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		logger.Error("Ошибка запроса сообщений чата Avito %s: %v", chatID, err, c.userID)
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		logger.Error("Avito API вернул ошибку при получении сообщений чата %s: %s", chatID, string(body), c.userID)
		return nil, fmt.Errorf("avito api error: status %d", resp.StatusCode)
	}

	var messages domain.MessagesResponse
	if err := json.NewDecoder(resp.Body).Decode(&messages); err != nil {
		logger.Error("Ошибка декодирования сообщений чата %s: %v", chatID, err, c.userID)
		return nil, fmt.Errorf("не удалось декодировать ответ: %w", err)
	}

	return messages, nil
}

// MarkChatAsRead помечает чат как прочитанный
// POST /messenger/v1/accounts/{user_id}/chats/{chat_id}/read
func (c *Client) MarkChatAsRead(chatID string) error {
	// Получаем валидный токен и AvitoUserID одновременно
	accessToken, avitoUserID, err := c.GetValidTokenWithUserID()
	if err != nil {
		logger.Error("Ошибка получения валидного токена для MarkChatAsRead: %v", err, c.userID)
		return fmt.Errorf("не удалось получить токен: %w", err)
	}

	// Формируем URL
	url := fmt.Sprintf("%s/accounts/%s/chats/%s/read", domain.AvitoMessengerV1, avitoUserID, chatID)

	// Отправляем POST запрос напрямую с уже полученным токеном
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return fmt.Errorf("ошибка создания запроса: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		logger.Error("Ошибка пометки чата %s как прочитанного: %v", chatID, err, c.userID)
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		logger.Error("Avito API вернул ошибку при пометке чата %s как прочитанного: %s", chatID, string(body), c.userID)
		return fmt.Errorf("avito api error: status %d", resp.StatusCode)
	}

	logger.Debug("✅ Чат %s помечен как прочитанным", chatID, c.userID)
	return nil
}

// handleIncomingMessage обрабатывает входящее сообщение от Avito
func (c *Client) handleIncomingMessage(payload domain.WebhookPayload) error {
	startedAt := time.Now()
	chatID := payload.Payload.ChatID
	message := payload.Payload.Message
	metrics.IncMessagesReceived(c.userID, message.Type)

	// Игнорируем исходящие сообщения
	if message.Direction != "in" {
		metrics.IncMessagesIgnored(c.userID, "outgoing")
		return nil
	}

	// Логируем входящее сообщение
	logger.Debug("Входящее сообщение от '%s' (chat=%s, msgID=%s): \"%s\"",
		message.GetAuthorName(), chatID, message.ID, message.GetText(), c.userID)

	respID, err := convertChatIDToDialogID(chatID) // Используем hash от chatID как respID
	if err != nil {
		return err
	}

	// Проверяем идемпотентность
	if data, ok := c.respondents.Load(respID); ok {
		respData := data.(*RespondentData)
		if respData.LastMessageID == message.ID {
			metrics.IncMessagesIgnored(c.userID, "duplicate")
			logger.Debug("Сообщение %s уже обработано, пропускаем", message.ID, c.userID)
			return nil
		}
	}

	// Обновляем данные респондента
	c.respondents.Store(respID, &RespondentData{
		LastMessageID: message.ID,
		LastSeen:      time.Now(),
		ChatID:        chatID,
		AuthorID:      message.Author.ID,
		AuthorName:    message.GetAuthorName(),
	})

	// Создаем массив файлов из изображений
	var files []model.FileUpload
	for _, img := range message.Content.Images {
		imageURL := img.Sizes.Origin.URL
		if imageURL == "" {
			continue
		}

		// Определяем MimeType по расширению
		mimeType := "image/jpeg"
		ext := path.Ext(imageURL)
		switch ext {
		case ".png":
			mimeType = "image/png"
		case ".webp":
			mimeType = "image/webp"
		case ".gif":
			mimeType = "image/gif"
		}

		files = append(files, model.FileUpload{
			Name:     path.Base(imageURL),
			URL:      imageURL,
			MimeType: mimeType,
		})
	}

	// Проверяем первое взаимодействие
	_, exists := c.knownUsers.Load(respID)
	if !exists {
		// Инициализируем новую сессию для респондента
		if err := c.initializeResponderSession(respID, message.GetAuthorName(), chatID); err != nil {
			metrics.IncMessagesProcessed(c.userID, "session_init_error")
			logger.Error("Ошибка инициализации сессии респондента: %v", err, c.userID)
			return err
		}
	}

	// Получаем каналы пользователя
	usrCh, err := c.mod.GetCh(respID)
	if err != nil {
		metrics.IncMessagesProcessed(c.userID, "channel_error")
		logger.Error("Ошибка получения канала: %v", err, c.userID)
		return err
	}

	// Создаем сообщение для модели
	content := model.AssistResponse{Message: message.GetText()}
	authorName := message.GetAuthorName()
	msg := c.mod.NewMessage(model.Operator{}, "user", &content, &authorName, files...)

	// Отправляем в канал модели
	if err := usrCh.SendToRx(msg); err != nil {
		metrics.IncMessagesProcessed(c.userID, "send_to_model_error")
		logger.Error("Ошибка отправки сообщения в модель: %v", err, c.userID)
		return err
	}

	// Отправляем в CRM если настроено
	if c.crm != nil {
		firstInteraction := !exists

		// Формируем identifier для CRM с приоритетом: nickname > ID
		identifier := message.GetAuthorName()
		if identifier == "" || identifier == "Unknown" {
			identifier = fmt.Sprintf("avito_%d", message.Author.ID)
		}

		csg := c.crm.MSG("user", message.GetAuthorName(), message.GetText()).
			WithAltContact(identifier).
			NewDialog(firstInteraction)

		if len(files) > 0 {
			fileNames := make([]string, len(files))
			for i, f := range files {
				fileNames[i] = f.Name
			}
			csg.WithFiles(fileNames...)
		}

		if err := c.crm.SendMessage(csg); err != nil {
			metrics.ObserveCRMRequest(c.userID, "outbound", "error", startedAt)
			logger.Error("Ошибка отправки в CRM: %v", err, c.userID)
		} else {
			metrics.ObserveCRMRequest(c.userID, "outbound", "success", startedAt)
		}
	}
	metrics.ObserveMessageProcessing(c.userID, "incoming_message", startedAt)
	metrics.IncMessagesProcessed(c.userID, "success")
	metrics.TrackActiveDialogs(c.userID, syncMapLen(&c.respondents))

	return nil
}

// initializeResponderSession инициализирует новую сессию для респондента
func (c *Client) initializeResponderSession(respID uint64, respName string, chatID string) error {
	// Получаем ID диалога (Type = 3 для Avito согласно chat_type)
	dialogId, err := c.db.GetOrSetTreadAndResponder(c.userID, respID, respName, comdom.Avito)
	if err != nil {
		return fmt.Errorf("ошибка при создании диалога: %w", err)
	}

	// Создаём/получаем модель пользователя
	usrMod, err := c.mod.GetOrSetRespGPT(*c.assist, dialogId, respID, respName)
	if err != nil {
		logger.Error("Ошибка при создании модели пользователя: %v", err, c.userID)
		return err
	}

	// Получаем каналы пользователя
	usrCh, err := c.mod.GetCh(respID)
	if err != nil {
		logger.Error("Ошибка при получении канала пользователя: %v", err, c.userID)
		return err
	}

	// Отмечаем первое взаимодействие
	c.knownUsers.Store(respID, true)

	// Отправляю сообщение о первом взаимодействии
	go c.sendFirstContactMessage(respID, respName)

	// Запускаем слушателя ответов
	go c.startResponseListener(respID, chatID)

	// Отправляем данные в канал запуска
	startCh := model.StartCh{
		Ctx:      c.ctx,
		ChName:   comdom.Avito,
		Model:    usrMod,
		Channel:  usrCh,
		ThreadId: dialogId,
		RespId:   respID,
	}

	select {
	case StartCh <- startCh:
		logger.Debug("Сессия успешно инициализирована для респондента в Avito chat %s", chatID, c.userID)
		metrics.ObserveUserChannelInit(c.userID, "success", time.Now())
	default:
		logger.Warn("Ошибка при отправке данных в StartCh", c.userID)
	}

	return nil
}

func syncMapLen(m *sync.Map) int {
	count := 0
	m.Range(func(_, _ interface{}) bool {
		count++
		return true
	})
	return count
}

// startResponseListener слушает ответы от модели и отправляет их в Avito
func (c *Client) startResponseListener(respID uint64, chatID string) {
	usrCh, err := c.mod.GetCh(respID)
	if err != nil {
		logger.Error("Ошибка получения канала для слушателя: %v", err, c.userID)
		return
	}

	for {
		select {
		case <-c.ctx.Done():
			return
		case msg, ok := <-usrCh.TxCh:
			if !ok {
				logger.Debug("TxCh закрыт для респондента %d", respID, c.userID)
				return
			}

			// Обрабатываем только ответы ассистента
			if msg.Type != "assist" {
				continue
			}

			// Логируем ответ AI агента
			logger.Debug("Ответ AI агента для '%s' (chat=%s): \"%s\"",
				usrCh.RespName, chatID, msg.Content.Message, c.userID)

			// Отправляем текст сообщения
			if msg.Content.Message != "" {
				if err := c.SendTextMessage(chatID, msg.Content.Message); err != nil {
					logger.Error("Ошибка отправки текста в Avito: %v", err, c.userID)
				} else {
					logger.Debug("Сообщение успешно отправлено в Avito (chat=%s)", chatID, c.userID)
				}
			}

			// Отправляем изображения
			if msg.Content.Action.SendFiles != nil {
				for _, file := range msg.Content.Action.SendFiles {
					// Проверяем что это изображение и у него есть URL
					if file.URL != "" && isImageType(file.Type) {
						if err := c.SendImageMessage(chatID, file.URL); err != nil {
							logger.Error("Ошибка отправки изображения в Avito: %v", err, c.userID)
						} else {
							logger.Debug("Изображение успешно отправлено в Avito: %s (chat=%s)", file.FileName, chatID, c.userID)
						}
					} else if !isImageType(file.Type) {
						logger.Warn("Пропуск не-изображения (Avito не поддерживает): %s", file.FileName, c.userID)
					}
				}
			}

			// Отправляем в CRM
			if c.crm != nil {
				// Получаем данные респондента для извлечения AuthorName/AuthorID
				var identifier string
				if respDataInterface, exists := c.respondents.Load(respID); exists {
					respData := respDataInterface.(*RespondentData)
					// Приоритет: nickname > ID
					identifier = respData.AuthorName
					if identifier == "" || identifier == "Unknown" {
						identifier = fmt.Sprintf("avito_%d", respData.AuthorID)
					}
				} else {
					// Fallback на chatID если данные респондента не найдены
					identifier = chatID
				}

				csg := c.crm.MSG("assist", usrCh.RespName, msg.Content.Message).
					WithAltContact(identifier).
					SetMeta(msg.Content.Meta)

				if msg.Content.Action.SendFiles != nil {
					fileNames := make([]string, 0)
					for _, file := range msg.Content.Action.SendFiles {
						fileNames = append(fileNames, file.FileName)
					}
					if len(fileNames) > 0 {
						csg.WithFiles(fileNames...)
					}
				}

				if err := c.crm.SendMessage(csg); err != nil {
					logger.Error("Ошибка отправки ответа в CRM: %v", err, c.userID)
				}
			}
		}
	}
}

// cleanupStaleRespondents периодически очищает неактивных респондентов
func (c *Client) cleanupStaleRespondents() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			c.respondents.Range(func(key, value interface{}) bool {
				data := value.(*RespondentData)
				if now.Sub(data.LastSeen) > domain.AvitoRespondentTTL {
					c.respondents.Delete(key)
					logger.Debug("Удален неактивный респондент %v", key, c.userID)
				}
				return true
			})
		}
	}
}

// pollUnreadChats периодически проверяет непрочитанные чаты и обрабатывает новые сообщения
func (c *Client) pollUnreadChats() {
	ticker := time.NewTicker(domain.AvitoPollingInterval)
	defer ticker.Stop()

	logger.Info("Запущен polling непрочитанных чатов для пользователя", c.userID)

	for {
		select {
		case <-c.ctx.Done():
			logger.Info("Остановлен polling непрочитанных чатов для пользователя", c.userID)
			return
		case <-ticker.C:
			c.checkAndProcessUnreadChats()
		}
	}
}

// checkInitialUnreadChats выполняет однократную проверку непрочитанных чатов при старте клиента
func (c *Client) checkInitialUnreadChats() {
	// Небольшая задержка перед первой проверкой для завершения инициализации
	time.Sleep(2 * time.Second)
	logger.Info("Выполняю начальную проверку непрочитанных чатов для пользователя", c.userID)
	c.checkAndProcessUnreadChats()
}

// checkAndProcessUnreadChats проверяет все активные чаты и обрабатывает новые сообщения
func (c *Client) checkAndProcessUnreadChats() {
	// Получаем список ВСЕХ активных чатов (не только непрочитанных!)
	// Важно: даже "прочитанный" чат может содержать новые необработанные сообщения
	chats, err := c.GetChats(false)
	if err != nil {
		logger.Error("Ошибка получения чатов при polling: %v", err, c.userID)
		return
	}

	if len(chats) == 0 {
		logger.Debug("Нет активных чатов для пользователя", c.userID)
		return
	}

	logger.Debug("Найдено %d активных чатов для пользователя, проверяю на новые сообщения...", len(chats), c.userID)
	processedCount := 0

	for _, chat := range chats {
		// Обрабатываем только чаты с входящими сообщениями
		if chat.LastMessage == nil || chat.LastMessage.Direction != "in" {
			continue
		}

		chatID := chat.ID
		respID, err := convertChatIDToDialogID(chatID)
		if err != nil {
			logger.Error("Ошибка обработки сообщения в активном чате %w", err)
			continue
		}

		// Проверяем, не обработано ли уже последнее сообщение
		if data, ok := c.respondents.Load(respID); ok {
			respData := data.(*RespondentData)
			if respData.LastMessageID == chat.LastMessage.ID {
				// Это сообщение уже обработано, пропускаем этот чат
				continue
			}
			logger.Debug("Обнаружено новое сообщение в чате %s (старое=%s, новое=%s)",
				chatID, respData.LastMessageID, chat.LastMessage.ID, c.userID)
		} else {
			logger.Debug("Новый чат обнаружен: %s с сообщением %s", chatID, chat.LastMessage.ID, c.userID)
		}

		// Пытаемся получить сообщения из чата
		// Примечание: этот метод требует платную подписку на "API мессенджера" Avito
		messages, err := c.GetChatMessages(chatID, 10)
		if err != nil {
			// Проверяем код ошибки - если 402, значит нет подписки на API
			if err.Error() == "avito api error: status 402" {
				logger.Warn("API мессенджера недоступен (нет подписки). Обрабатываю последнее сообщение из списка чатов...", c.userID)

				// Обрабатываем только последнее сообщение из chat.LastMessage
				// Это работает БЕСПЛАТНО через API v2/chats
				if chat.LastMessage != nil {
					// Создаем payload для обработки последнего сообщения
					payload := domain.WebhookPayload{
						Type: "message",
						Payload: struct {
							ChatID  string         `json:"chat_id"`
							Message domain.Message `json:"message"`
						}{
							ChatID:  chatID,
							Message: *chat.LastMessage,
						},
					}

					// Обрабатываем сообщение через существующий метод
					if err := c.handleIncomingMessage(payload); err != nil {
						logger.Error("Ошибка обработки последнего сообщения %s из чата %s: %v", chat.LastMessage.ID, chatID, err, c.userID)
					} else {
						processedCount++
						logger.Debug("Обработано последнее сообщение %s из чата %s (режим без подписки)", chat.LastMessage.ID, chatID, c.userID)

						// Помечаем чат как прочитанный после обработки сообщения
						if err := c.MarkChatAsRead(chatID); err != nil {
							logger.Warn("Не удалось пометить чат %s как прочитанный: %v", chatID, err, c.userID)
						}
					}
				}
				continue
			}

			logger.Error("Ошибка получения сообщений чата %s при polling: %v", chatID, err, c.userID)
			continue
		}

		// Если подписка есть - обрабатываем все сообщения в обратном порядке (от старых к новым)
		for i := len(messages) - 1; i >= 0; i-- {
			msg := messages[i]

			// Пропускаем исходящие сообщения
			if msg.Direction != "in" {
				continue
			}

			// Проверяем идемпотентность
			if data, ok := c.respondents.Load(respID); ok {
				respData := data.(*RespondentData)
				if respData.LastMessageID == msg.ID {
					// Это и все предыдущие сообщения уже обработаны
					logger.Debug("Сообщение %s уже было обработано ранее, останавливаю обработку чата %s", msg.ID, chatID, c.userID)
					break
				}
			}

			// Создаем payload для обработки сообщения
			payload := domain.WebhookPayload{
				Type: "message",
				Payload: struct {
					ChatID  string         `json:"chat_id"`
					Message domain.Message `json:"message"`
				}{
					ChatID:  chatID,
					Message: msg,
				},
			}

			// Обрабатываем сообщение через существующий метод
			if err := c.handleIncomingMessage(payload); err != nil {
				logger.Error("Ошибка обработки сообщения %s из чата %s при polling: %v", msg.ID, chatID, err, c.userID)
			} else {
				processedCount++
				logger.Debug("Обработано сообщение %s из чата %s при polling", msg.ID, chatID, c.userID)
			}
		}

		// Помечаем чат как прочитанный после обработки всех сообщений
		if err := c.MarkChatAsRead(chatID); err != nil {
			logger.Warn("Не удалось пометить чат %s как прочитанный: %v", chatID, err, c.userID)
			// Не критично, продолжаем работу
		}
	}
}

// Shutdown останавливает клиента
func (c *Client) Shutdown() {
	c.cancel()
}

// SetOperatorMode выставляет/снимает режим оператора для конкретного диалога
func (u *User) SetOperatorMode(dialogId uint64, operator bool) {
	if operator {
		u.operatorModeByDialog.Store(dialogId, true)
	} else {
		u.operatorModeByDialog.Delete(dialogId)
	}
}

// IsOperatorMode проверяет, включен ли режим оператора для диалога
func (u *User) IsOperatorMode(dialogId uint64) bool {
	if mode, ok := u.operatorModeByDialog.Load(dialogId); ok {
		return mode.(bool)
	}
	return false
}

// DisableOperatorMode отключает режим оператора и уведомляет AI-модель
func (u *User) DisableOperatorMode(userId uint32, dialogId uint64, silent ...bool) error {
	// Определяем значение silent (по умолчанию false)
	isSilent := false
	if len(silent) > 0 {
		isSilent = silent[0]
	}

	// 1. Выключаем режим оператора
	u.SetOperatorMode(dialogId, false)
	logger.Debug("Выключен режим оператора для диалога %d", dialogId, userId)

	// 2. Находим respId по dialogId
	respId, err := u.mod.GetRespIdByDialogID(dialogId)
	if err != nil {
		logger.Error("Не удалось найти respId для dialogId %d: %v", dialogId, err)
		return err
	}

	// 3. Получаем клиента Avito для пользователя
	clientInterface, ok := u.clients.Load(userId)

	if !ok {
		logger.Error("Клиент Avito не найден для userId %d", userId)
		return fmt.Errorf("клиент не найден")
	}

	client := clientInterface.(*Client)

	// 4. Получаем chatID из respondents по respId
	respDataInterface, exists := client.respondents.Load(respId)
	if !exists {
		logger.Error("Не найдены данные респондента для respId %d", respId, userId)
		return fmt.Errorf("данные респондента не найдены")
	}

	respData := respDataInterface.(*RespondentData)
	chatID := respData.ChatID

	// 5. Отправляем сообщение пользователю только если не silent режим
	if !isSilent {
		//messageText := "Оператор отключился. Маруся AI снова с вами!"
		if err := client.SendTextMessage(
			chatID,
			u.end.TranslateMessageWithUserID(userId, "operator.disconnected")); err != nil {
			logger.Error("Ошибка отправки сообщения о выключении оператора в Avito chat %s: %v", chatID, err, userId)
		}
	}

	// 6. Уведомляем AI-модель о возобновлении работы
	usrCh, err := u.mod.GetCh(respId)
	if err != nil {
		logger.Error("Каналы для respId %d не найдены при отключении оператора: %v", respId, err, userId)
		return err
	}

	systemName := "assist"
	operatorOffMsg := u.mod.NewMessage(
		model.Operator{SetOperator: false, Operator: false},
		"assist",
		//&model.AssistResponse{Message: "Режим оператора отключен, возобновляю работу AI"},
		&model.AssistResponse{Message: u.end.TranslateMessageWithUserID(userId, "operator.mode.is.disabled")},
		&systemName,
	)

	if err := u.trySendToRxCh(usrCh, operatorOffMsg); err != nil {
		logger.Error("Не удалось отправить системное сообщение в модель о выключении оператора: %v", err)
	}

	// 7. Отправляем в CRM если настроено
	if client.crm != nil {
		// Получаем identifier из респондента
		var identifier string
		if respData.AuthorName != "" && respData.AuthorName != "Unknown" {
			identifier = respData.AuthorName
		} else if respData.AuthorID != 0 {
			identifier = fmt.Sprintf("avito_%d", respData.AuthorID)
		} else {
			identifier = chatID
		}

		// TODO добавить локализацию!
		//csg := client.crm.MSG("system", "Система", "Режим оператора отключен").
		csg := client.crm.MSG("system",
			u.end.TranslateMessageWithUserID(userId, "system"),
			u.end.TranslateMessageWithUserID(userId, "mode.operator.disabled")).
			WithAltContact(identifier)

		if err := client.crm.SendMessage(csg); err != nil {
			logger.Error("Ошибка отправки в CRM о выключении оператора: %v", err, userId)
		}
	}

	return nil
}

// trySendToRxCh пытается отправить сообщение в RxCh с проверкой ошибок
func (u *User) trySendToRxCh(usrCh *model.Ch, msg model.Message) error {
	if err := usrCh.SendToRx(msg); err != nil {
		return fmt.Errorf("не удалось отправить сообщение в RxCh: %w", err)
	}
	return nil
}

// SetOperator устанавливает оператора для User
func (u *User) SetOperator(op Operator) {
	u.op = op
}

// Shutdown останавливает все клиенты
func (u *User) Shutdown() {
	logger.Info("Avito: получен сигнал завершения, закрытие клиентов...")
	u.cancel()

	u.clients.Range(func(key, value interface{}) bool {
		client := value.(*Client)
		client.Shutdown()
		return true
	})

	logger.Info("Avito: все клиенты остановлены")
}

// StartClients запускает клиентов для всех активных пользователей
func (u *User) StartClients() error {
	users, err := u.db.GetAvitoUsers(u.ctx)
	if err != nil {
		logger.Error("Ошибка получения пользователей Avito: %v", err)
		return err
	}

	if len(users) == 0 {
		logger.Info("Нет активных пользователей Avito")
		return nil
	}

	for _, user := range users {
		userId := uint32(user.UserID)

		// Создаем модель ассистента
		assist := &model.Assistant{
			UserID:     userId,
			AssistName: user.AssistName,
			AssistId:   user.AssistantID,
			Provider:   user.Provider,
			Metas: model.Target{
				MetaAction: user.MetaAction,
				Triggers:   user.Triggers,
			},
			Events: model.Notifications{
				Start:  user.Events.Start,
				End:    user.Events.End,
				Target: user.Events.Target,
			},
			Limit:  user.AskLimit,
			Espero: user.Espero,
			Ignore: user.Ignore,
		}

		// Создаем клиента
		client := NewClient(u, userId, assist)

		u.clients.Store(userId, client)

		logger.Info("Avito клиент запущен для пользователя", userId)
	}

	logger.Info("Запущено %d Avito клиентов", len(users))
	return nil
}

// StartUserClient запускает Avito клиента для конкретного пользователя
func (u *User) StartUserClient(userId uint32) error {
	// Проверяем, не запущен ли уже клиент
	if _, exists := u.clients.Load(userId); exists {
		logger.Warn("Avito клиент уже запущен для пользователя", userId)
		return fmt.Errorf("клиент уже запущен")
	}

	// Проверяем наличие токена Avito
	_, err := u.db.GetAvitoToken(u.ctx, userId, u.userMasterKey(userId))
	if err != nil {
		logger.Error("У пользователя нет Avito токена", userId)
		return fmt.Errorf("авторизация Avito не выполнена")
	}

	// Получаем полные данные пользователя из БД
	userDetails, err := u.db.GetAvitoUser(u.ctx, userId)
	if err != nil {
		logger.Error("Ошибка получения данных пользователя: %v", err, userId)
		return fmt.Errorf("не удалось получить данные пользователя: %w", err)
	}

	if userDetails == nil {
		logger.Error("Пользователь не найден в БД", userId)
		return fmt.Errorf("пользователь не найден")
	}

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

	// Создаем и запускаем клиента
	client := NewClient(u, userId, assist)

	u.clients.Store(userId, client)

	logger.Info("Avito клиент успешно запущен для пользователя", userId)
	return nil
}

// StopUserClient останавливает Avito клиента для конкретного пользователя
func (u *User) StopUserClient(userId uint32) error {
	// Проверяем существование клиента
	clientInterface, exists := u.clients.Load(userId)
	if !exists {
		logger.Warn("Avito клиент не найден для остановки", userId)
		return fmt.Errorf("клиент не запущен")
	}

	client := clientInterface.(*Client)

	// Останавливаем клиента
	client.Shutdown()

	// Удаляем из карты клиентов
	u.clients.Delete(userId)

	logger.Info("Avito клиент успешно остановлен для пользователя", userId)
	return nil
}
