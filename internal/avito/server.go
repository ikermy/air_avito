package avito

import (
	"air_avito/internal/delivery/http"
)

// StartServer запускает Fiber веб-сервер для webhooks и OAuth.
func (u *User) StartServer() error {
	return http.StartAvitoServer(u.ctx, http.AvitoHandlers{
		ExtractUID: u.extractUID,
		Status:     u.StatusHandler, AuthURL: u.AuthURLHandler, Enable: u.EnableHandler,
		Disable: u.DisableHandler, Chats: u.ChatsHandler, Subscriptions: u.SubscriptionsHandler,
		Subscribe: u.SubscribeHandler, Unsubscribe: u.UnsubscribeHandler,
		Available: u.AvailableHandler, AuthCallback: u.AuthCallbackHandler, Webhook: u.WebhookHandler,
	})
}
