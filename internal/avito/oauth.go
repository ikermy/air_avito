package avito

import (
	"air_avito/internal/domain"
	"air_avito/internal/metrics"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ikermy/air_common/pkg/mode"
	"github.com/ikermy/air_logger/v2/pkg/logger"
)

const (
	avitoAuthURL  = "https://www.avito.ru/oauth"
	avitoTokenURL = "https://api.avito.ru/token"
)

// normalizeRedirectURL извлекает только домен (хост) из любого формата URL
// Примеры:
//
//	"https://example.com/open/avito/auth/callback" -> "example.com"
//	"example.com/path" -> "example.com"
//	"https://example.com/" -> "example.com"
//	"example.com" -> "example.com"
func normalizeRedirectURL(rawURL string) string {
	// Убираем пробелы
	rawURL = strings.TrimSpace(rawURL)

	// Пытаемся распарсить как URL
	parsedURL, err := url.Parse(rawURL)
	if err != nil || parsedURL.Host == "" {
		// Если не удалось распарсить или нет хоста, пробуем добавить схему
		if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
			rawURL = "https://" + rawURL
			parsedURL, err = url.Parse(rawURL)
			if err != nil {
				// В крайнем случае возвращаем как есть
				return strings.TrimSuffix(strings.TrimPrefix(rawURL, "https://"), "/")
			}
		}
	}

	// Извлекаем только хост (домен)
	host := parsedURL.Host
	if host == "" {
		// Если всё равно нет хоста, берём path как хост
		host = strings.TrimPrefix(parsedURL.Path, "/")
		host = strings.Split(host, "/")[0]
	}

	return host
}

// tokenRefreshMutexes карта мьютексов для синхронизации обновления токенов по userId
var (
	tokenRefreshMutexes = make(map[uint32]*sync.Mutex)
	mutexMapLock        sync.RWMutex
)

// getOrCreateMutex получает или создает мьютекс для конкретного пользователя
func getOrCreateMutex(userId uint32) *sync.Mutex {
	mutexMapLock.RLock()
	mu, exists := tokenRefreshMutexes[userId]
	mutexMapLock.RUnlock()

	if exists {
		return mu
	}

	mutexMapLock.Lock()
	defer mutexMapLock.Unlock()

	// Двойная проверка после получения блокировки
	if mu, exists := tokenRefreshMutexes[userId]; exists {
		return mu
	}

	mu = &sync.Mutex{}
	tokenRefreshMutexes[userId] = mu
	return mu
}

// OAuthTokenResponse ответ от Avito OAuth API
type OAuthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"` // seconds
}

// GetValidToken получает валидный access token, обновляя при необходимости
func (c *Client) GetValidToken() (string, error) {
	token, err := c.db.GetAvitoToken(c.ctx, c.userID, c.mk)
	if err != nil {
		logger.Error("Ошибка получения токена из БД: %v", err, c.userID)
		return "", err
	}

	// Проверяем, не истек ли токен (с запасом 5 минут)
	if time.Now().Add(5 * time.Minute).After(token.Expiry) {
		logger.Debug("Access token истекает, обновляем...", c.userID)
		newAccessToken, newExpiry, err := c.RefreshAccessToken(token.RefreshToken)
		if err != nil {
			logger.Error("Ошибка обновления токена: %v", err, c.userID)
			return "", err
		}

		// Обновляем токен в БД
		if err := c.db.UpdateAvitoToken(c.ctx, c.userID, c.mk, newAccessToken, newExpiry); err != nil {
			logger.Error("Ошибка сохранения обновленного токена: %v", err, c.userID)
			return "", err
		}

		return newAccessToken, nil
	}

	return token.AccessToken, nil
}

// GetValidTokenWithUserID получает валидный access token и AvitoUserID, обновляя токен при необходимости
func (c *Client) GetValidTokenWithUserID() (accessToken string, avitoUserID string, err error) {
	startedAt := time.Now()
	// Захватываем мьютекс для предотвращения параллельного обновления токена
	mu := getOrCreateMutex(c.userID)
	mu.Lock()
	defer mu.Unlock()

	logger.Debug("[GetValidTokenWithUserID] Начало получения токена", c.userID)

	token, err := c.db.GetAvitoToken(c.ctx, c.userID, c.mk)
	if err != nil {
		metrics.IncDecryptErrors(c.userID, "token_load")
		logger.Error("[GetValidTokenWithUserID] Ошибка получения токена из БД: %v", err, c.userID)
		return "", "", err
	}

	// Проверяем что AvitoUserID не пустой
	if token.AvitoUserID == "" {
		metrics.IncDecryptErrors(c.userID, "missing_avito_user_id")
		logger.Error("[GetValidTokenWithUserID] AvitoUserID пустой в БД! Токен поврежден, требуется повторная авторизация", c.userID)
		return "", "", fmt.Errorf("AvitoUserID пустой, требуется повторная авторизация через OAuth")
	}

	timeUntilExpiry := time.Until(token.Expiry)
	logger.Debug("[GetValidTokenWithUserID] Токен истекает через %v (в %s), AvitoUserID=%s",
		timeUntilExpiry,
		token.Expiry.Format("2006-01-02 15:04:05"),
		token.AvitoUserID, c.userID)

	// Проверяем, не истек ли токен (с запасом 5 минут)
	if time.Now().Add(5 * time.Minute).After(token.Expiry) {
		logger.Debug("[GetValidTokenWithUserID] Токен истекает, начинаю обновление...", c.userID)

		newAccessToken, newExpiry, err := c.RefreshAccessToken(token.RefreshToken)
		if err != nil {
			metrics.IncReconnects(c.userID, "refresh_error")
			logger.Error("[GetValidTokenWithUserID] Ошибка обновления токена: %v", err, c.userID)
			return "", "", err
		}

		// Проверяем что новый токен не пустой
		if newAccessToken == "" {
			metrics.IncReconnects(c.userID, "empty_token")
			logger.Error("[GetValidTokenWithUserID] Получен пустой access token от Avito!", c.userID)
			return "", "", fmt.Errorf("получен пустой токен от Avito")
		}

		// Проверяем что новый токен не истекает мгновенно
		tokenLifetime := time.Until(newExpiry)
		if tokenLifetime < 1*time.Minute {
			metrics.IncReconnects(c.userID, "short_lived_token")
			logger.Error("[GetValidTokenWithUserID] Новый токен истекает слишком быстро: через %v (expiry=%s)",
				tokenLifetime, newExpiry.Format("2006-01-02 15:04:05"), c.userID)
			return "", "", fmt.Errorf("токен истекает мгновенно: %v", tokenLifetime)
		}

		logger.Debug("[GetValidTokenWithUserID] Новый токен получен (длина=%d), истекает в %s (через %v)",
			len(newAccessToken),
			newExpiry.Format("2006-01-02 15:04:05"),
			tokenLifetime, c.userID)

		// Обновляем токен в БД
		if err := c.db.UpdateAvitoToken(c.ctx, c.userID, c.mk, newAccessToken, newExpiry); err != nil {
			metrics.IncReconnects(c.userID, "save_error")
			logger.Error("[GetValidTokenWithUserID] Ошибка сохранения обновленного токена: %v", err, c.userID)
			return "", "", err
		}

		// Безопасное логирование токена
		tokenPreview := newAccessToken
		if len(newAccessToken) > 20 {
			tokenPreview = newAccessToken[:20] + "..."
		}
		logger.Debug("[GetValidTokenWithUserID] Токен успешно обновлен в БД, preview: %s", tokenPreview, c.userID)
		metrics.IncReconnects(c.userID, "success")
		metrics.ObserveDuration(metrics.MessageProcessingDuration.WithLabelValues(metrics.BotLabel(c.userID), "token_refresh"), startedAt)

		// Возвращаем новый токен и AvitoUserID (который не меняется)
		return newAccessToken, token.AvitoUserID, nil
	}

	// Безопасное логирование токена
	tokenPreview := token.AccessToken
	if len(token.AccessToken) > 20 {
		tokenPreview = token.AccessToken[:20] + "..."
	}
	logger.Debug("[GetValidTokenWithUserID] Токен актуален, preview: %s", tokenPreview, c.userID)

	return token.AccessToken, token.AvitoUserID, nil
}

// RefreshAccessToken обновляет access token используя refresh token
func (c *Client) RefreshAccessToken(refreshToken string) (string, time.Time, error) {
	data := url.Values{}
	data.Set("grant_type", "refresh_token")
	data.Set("refresh_token", refreshToken)
	data.Set("client_id", c.clientID)
	data.Set("client_secret", c.clientSecret)

	resp, err := http.PostForm(avitoTokenURL, data)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("ошибка запроса обновления токена: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			logger.Error("ошибка закрытия resp.Body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		logger.Error("Avito OAuth refresh failed: %s", string(body), c.userID)
		return "", time.Time{}, fmt.Errorf("avito вернул статус %d", resp.StatusCode)
	}

	var tokenResp OAuthTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", time.Time{}, fmt.Errorf("ошибка парсинга ответа: %w", err)
	}

	expiry := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	logger.Debug("Access token успешно обновлен", c.userID)

	return tokenResp.AccessToken, expiry, nil
}

// ExchangeCodeForToken обменивает authorization code на токены
func (u *User) ExchangeCodeForToken(code string, userId uint32, db DB, settings *domain.PendingOAuthSettings) error {
	if settings == nil {
		return fmt.Errorf("персональные настройки OAuth не переданы")
	}

	// RedirectUrlPrefix теперь содержит только домен (нормализован в AuthURLHandler)
	redirectURI := fmt.Sprintf("https://%s/open/avito/auth/callback", settings.RedirectURLPrefix)

	data := url.Values{}
	data.Set("grant_type", "authorization_code")
	data.Set("code", code)
	data.Set("client_id", settings.ClientID)
	data.Set("client_secret", settings.ClientSecret)
	data.Set("redirect_uri", redirectURI) // ВАЖНО: должен совпадать с OAuth URL

	logger.Debug("ExchangeCodeForToken: grant_type=%s, code=%s, client_id=%s, redirect_uri=%s",
		data.Get("grant_type"), code, settings.ClientID, redirectURI, userId)

	// Создаем HTTP клиент с таймаутом
	client := &http.Client{Timeout: 30 * time.Second}

	// Создаем POST запрос с явным указанием Content-Type
	req, err := http.NewRequest("POST", avitoTokenURL, bytes.NewBufferString(data.Encode()))
	if err != nil {
		logger.Error("Ошибка создания запроса: %v", err, userId)
		return fmt.Errorf("ошибка создания запроса: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		logger.Error("Ошибка обмена code на токен: %v", err, userId)
		return fmt.Errorf("ошибка запроса токена: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			logger.Error("ошибка закрытия resp.Body: %v", err)
		}
	}()

	// Читаем тело ответа для детального логирования
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		logger.Error("Ошибка чтения ответа от Avito: %v", readErr, userId)
		return fmt.Errorf("ошибка чтения ответа: %w", readErr)
	}

	if resp.StatusCode != http.StatusOK {
		logger.Error("Avito OAuth exchange failed: status=%d, response=%s", resp.StatusCode, string(body), userId)
		return fmt.Errorf("avito вернул статус %d: %s", resp.StatusCode, string(body))
	}

	logger.Debug("ExchangeCodeForToken: получен успешный ответ от Avito, парсинг токенов", userId)

	var tokenResp OAuthTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		logger.Error("Ошибка парсинга OAuth ответа: %v, body=%s", err, string(body), userId)
		return fmt.Errorf("ошибка парсинга ответа: %w", err)
	}

	logger.Debug("ExchangeCodeForToken: токены получены, запрос user info", userId)

	// Получаем информацию о пользователе Avito (включая avito_user_id)
	avitoUserID, err := getAvitoUserInfo(tokenResp.AccessToken, userId)
	if err != nil {
		logger.Error("Ошибка получения user info от Avito: %v", err, userId)
		return fmt.Errorf("не удалось получить Avito user ID: %w", err)
	}

	expiry := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)

	// Сохраняем токены в БД (теперь включая персональные настройки)
	token := domain.Token{
		AvitoUserID:       avitoUserID,
		AccessToken:       tokenResp.AccessToken,
		RefreshToken:      tokenResp.RefreshToken,
		TokenType:         tokenResp.TokenType,
		Expiry:            expiry,
		Scopes:            []string{"messenger:read", "messenger:write"},
		ClientID:          settings.ClientID,
		ClientSecret:      settings.ClientSecret,
		RedirectURLPrefix: settings.RedirectURLPrefix,
	}

	if err := db.SaveAvitoToken(u.ctx, userId, u.userMasterKey(userId), token); err != nil {
		logger.Error("Ошибка сохранения токенов в БД: %v", err, userId)
		return err
	}

	logger.Info("Avito OAuth успешно завершен, токены сохранены для Avito User ID: %s", avitoUserID, userId)
	return nil
}

// getAvitoUserInfo получает информацию о пользователе Avito через API
func getAvitoUserInfo(accessToken string, userId uint32) (string, error) {
	req, err := http.NewRequest("GET", "https://api.avito.ru/core/v1/accounts/self", nil)
	if err != nil {
		return "", fmt.Errorf("ошибка создания запроса: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ошибка запроса user info: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			logger.Error("ошибка закрытия resp.Body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		logger.Error("Avito user info request failed: %s", string(body), userId)
		return "", fmt.Errorf("avito вернул статус %d", resp.StatusCode)
	}

	var userInfo struct {
		ID   int64  `json:"id"`   // ID аккаунта Avito
		Name string `json:"name"` // Имя пользователя
	}

	if err := json.NewDecoder(resp.Body).Decode(&userInfo); err != nil {
		return "", fmt.Errorf("ошибка парсинга user info: %w", err)
	}

	if userInfo.ID == 0 {
		return "", fmt.Errorf("получен пустой user ID от Avito")
	}

	avitoUserID := fmt.Sprintf("%d", userInfo.ID)
	logger.Debug("Получен Avito User ID: %s, Name: %s", avitoUserID, userInfo.Name, userId)

	return avitoUserID, nil
}

// GenerateAuthURL генерирует URL для авторизации пользователя
func (u *User) GenerateAuthURL(userId uint32, redirectURI string, clientID string) string {
	params := url.Values{}
	params.Set("client_id", clientID)
	params.Set("response_type", "code")
	params.Set("redirect_uri", redirectURI)
	params.Set("scope", "user:read,messenger:read,messenger:write") // user:read для /accounts/self, messenger для чатов
	params.Set("state", fmt.Sprintf("%d", userId))                  // Передаем userId в state

	return fmt.Sprintf("%s?%s", avitoAuthURL, params.Encode())
}

// SendAPIRequest отправляет запрос к Avito API с авторизацией
func (c *Client) SendAPIRequest(method, urlPath string, body interface{}) (*http.Response, error) {
	token, err := c.GetValidToken()
	if err != nil {
		return nil, fmt.Errorf("ошибка получения токена: %w", err)
	}

	var reqBody io.Reader
	if body != nil {
		jsonData, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("ошибка сериализации тела запроса: %w", err)
		}
		reqBody = bytes.NewBuffer(jsonData)
	}

	req, err := http.NewRequest(method, urlPath, reqBody)
	if err != nil {
		return nil, fmt.Errorf("ошибка создания запроса: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ошибка выполнения запроса: %w", err)
	}

	// Проверяем rate limit
	if resp.StatusCode == http.StatusTooManyRequests {
		logger.Warn("Avito API rate limit exceeded", c.userID)

		if err := resp.Body.Close(); err != nil {
			logger.Error("ошибка закрытия resp.Body: %v", err)
		}

		return nil, fmt.Errorf("rate limit exceeded (429)")
	}

	return resp, nil
}

// GetChats получает список чатов пользователя через Avito Messenger API
// GET /messenger/v2/accounts/{user_id}/chats
func (u *User) GetChats(userId uint32, db DB, limit int, offset int, unreadOnly bool) (*domain.ChatsResponse, error) {
	// Получаем Avito user ID из БД
	token, err := db.GetAvitoToken(u.ctx, userId, u.userMasterKey(userId))
	if err != nil {
		return nil, fmt.Errorf("ошибка получения токена из БД: %w", err)
	}

	// Формируем URL с параметрами
	apiURL := fmt.Sprintf("%s/accounts/%s/chats", domain.AvitoMessengerV2, token.AvitoUserID)

	// Добавляем query параметры
	params := url.Values{}
	if limit > 0 {
		params.Set("limit", fmt.Sprintf("%d", limit))
	}
	if offset > 0 {
		params.Set("offset", fmt.Sprintf("%d", offset))
	}
	if unreadOnly {
		params.Set("unread_only", "true")
	}

	if len(params) > 0 {
		apiURL = fmt.Sprintf("%s?%s", apiURL, params.Encode())
	}

	client, err := u.GetClientByUserID(userId)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения клиента для userId=%d: %w", userId, err)
	}
	// Отправляем запрос
	resp, err := client.SendAPIRequest("GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("ошибка запроса списка чатов: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			logger.Error("ошибка закрытия resp.Body: %v", err)
		}
	}()

	// Проверяем статус ответа
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		logger.Error("Avito chats request failed: status=%d, response=%s", resp.StatusCode, string(body), userId)
		return nil, fmt.Errorf("avito вернул статус %d", resp.StatusCode)
	}

	// Парсим ответ
	var chatsResp domain.ChatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatsResp); err != nil {
		return nil, fmt.Errorf("ошибка парсинга ответа чатов: %w", err)
	}

	logger.Debug("Получено чатов: %d", len(chatsResp.Chats), userId)
	return &chatsResp, nil
}

// GetSubscriptions получает список webhook подписок
// POST /messenger/v1/subscriptions
func (u *User) GetSubscriptions(userId uint32) (*domain.SubscriptionsResponse, error) {
	apiURL := fmt.Sprintf("%s/subscriptions", domain.AvitoMessengerV1)

	client, err := u.GetClientByUserID(userId)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения клиента для userId=%d: %w", userId, err)
	}

	// Отправляем запрос
	resp, err := client.SendAPIRequest("POST", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("ошибка запроса списка подписок: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			logger.Error("ошибка закрытия resp.Body: %v", err)
		}
	}()

	// Проверяем статус ответа
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		logger.Error("Avito subscriptions request failed: status=%d, response=%s", resp.StatusCode, string(body), userId)
		return nil, fmt.Errorf("avito вернул статус %d", resp.StatusCode)
	}

	// Парсим ответ
	var subsResp domain.SubscriptionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&subsResp); err != nil {
		return nil, fmt.Errorf("ошибка парсинга ответа подписок: %w", err)
	}

	logger.Debug("Получено подписок: %d", len(subsResp.Subscriptions), userId)
	return &subsResp, nil
}

func (u *User) GetClientByUserID(userId uint32) (*Client, error) {
	// Получаю клиента по userId для логирования внутри SendAPIRequest
	clientInterface, ok := u.clients.Load(userId)
	if !ok {
		logger.Error("GetChats: клиент не найден", userId)
		return nil, fmt.Errorf("клиент не найден для userId=%d", userId)
	}
	client, ok := clientInterface.(*Client)
	if !ok {
		logger.Error("GetChats: некорректный тип клиента", userId)
		return nil, fmt.Errorf("некорректный тип клиента")
	}

	return client, nil
}

// SubscribeToWebhooks подписывается на webhook'и Avito Messenger
// POST /messenger/v3/webhook
func (u *User) SubscribeToWebhooks(userId uint32, db DB) error {
	// Получаем токен пользователя для извлечения персонального URL
	token, err := db.GetAvitoToken(u.ctx, userId, u.userMasterKey(userId))
	if err != nil {
		return fmt.Errorf("ошибка получения токена для userId=%d: %w", userId, err)
	}

	// Используем персональный URL префикс если он есть, иначе fallback на mode.RealHost
	webhookURL := ""
	if token.RedirectURLPrefix != "" {
		// Нормализуем URL на случай старых записей в БД
		normalizedDomain := normalizeRedirectURL(token.RedirectURLPrefix)
		webhookURL = fmt.Sprintf("https://%s/open/avito/webhook", normalizedDomain)
	} else {
		// mode.RealHost уже содержит протокол https://
		webhookURL = fmt.Sprintf("%s/open/avito/webhook", mode.GetRealHost())
	}

	reqBody := map[string]string{
		"url": webhookURL,
	}

	client, err := u.GetClientByUserID(userId)
	if err != nil {
		return fmt.Errorf("ошибка получения клиента для userId=%d: %w", userId, err)
	}

	apiUrl := fmt.Sprintf("%s/webhook", domain.AvitoMessengerV3)
	// Отправляем запрос
	resp, err := client.SendAPIRequest("POST", apiUrl, reqBody)
	if err != nil {
		return fmt.Errorf("ошибка запроса подписки на webhook'и: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			logger.Error("ошибка закрытия resp.Body: %v", err)
		}
	}()

	// Проверяем статус ответа
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		logger.Error("Avito webhook subscribe failed: status=%d, response=%s", resp.StatusCode, string(body), userId)
		return fmt.Errorf("avito вернул статус %d при подписке на webhook'и", resp.StatusCode)
	}

	logger.Info("Успешная подписка на Avito webhook'и: %s", webhookURL, userId)
	return nil
}

// UnsubscribeFromWebhooks отписывается от webhook'ов
// POST /messenger/v1/webhook/unsubscribe (для V3 используется тот же метод или отдельный эндпоинт)
func (u *User) UnsubscribeFromWebhooks(userId uint32, webhookURL string) error {
	apiURL := fmt.Sprintf("%s/webhook/unsubscribe", domain.AvitoMessengerV1)

	reqBody := domain.UnsubscribeRequest{
		URL: webhookURL,
	}

	client, err := u.GetClientByUserID(userId)
	if err != nil {
		return fmt.Errorf("ошибка получения клиента для userId=%d: %w", userId, err)
	}
	// Отправляем запрос
	resp, err := client.SendAPIRequest("POST", apiURL, reqBody)
	if err != nil {
		return fmt.Errorf("ошибка запроса отписки от webhook'ов: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			logger.Error("ошибка закрытия resp.Body: %v", err)
		}
	}()

	// Проверяем статус ответа
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		logger.Error("Avito webhook unsubscribe failed: status=%d, response=%s", resp.StatusCode, string(body), userId)
		return fmt.Errorf("avito вернул статус %d при отписке от webhook'ов", resp.StatusCode)
	}

	logger.Info("Успешная отписка от Avito webhook'а: %s", webhookURL, userId)
	return nil
}
