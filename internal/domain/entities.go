package domain

import (
	"encoding/json"
	"time"

	"github.com/ikermy/air-common/pkg/comdom"
)

// Token представляет OAuth токены Avito, хранимые в channels.Avito.
type Token struct {
	AvitoUserID       string    `json:"avito_user_id"`
	AccessToken       string    `json:"access_token"`
	RefreshToken      string    `json:"refresh_token"`
	TokenType         string    `json:"token_type"`
	Expiry            time.Time `json:"expiry"`
	Scopes            []string  `json:"scopes"`
	ClientID          string    `json:"client_id"`
	ClientSecret      string    `json:"client_secret"`
	RedirectURLPrefix string    `json:"redirect_url_prefix"`
}

// Notifications события уведомлений.
type Notifications struct {
	Start  bool
	End    bool
	Target bool
}

// UserDetails представляет бизнес-данные пользователя для Avito-интеграции.
type UserDetails struct {
	UserID int64
	//Avito        string
	AvitoEnabled bool
	AssistName   string
	AssistantID  string
	Provider     comdom.ProviderType
	MetaAction   string
	Triggers     []string
	Espero       uint8
	AskLimit     uint32
	Ignore       bool
	Events       Notifications
}

// PendingOAuthSettings временные настройки OAuth до завершения авторизации.
type PendingOAuthSettings struct {
	ClientID          string
	ClientSecret      string
	RedirectURLPrefix string
	CreatedAt         time.Time
}

// RespondentData хранит данные о респонденте для идемпотентности.
type RespondentData struct {
	LastMessageID string
	LastSeen      time.Time
	ChatID        string
	AuthorID      int64
	AuthorName    string
}

// WebhookPayload представляет входящий webhook от Avito.
type WebhookPayload struct {
	Type    string `json:"type"`
	Payload struct {
		ChatID  string  `json:"chat_id"`
		Message Message `json:"message"`
	} `json:"payload"`
}

// Message представляет сообщение от Avito.
type Message struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Created   int64  `json:"created"`
	Direction string `json:"direction"`
	AuthorID  int64  `json:"author_id"`
	Author    struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"author"`
	Content struct {
		Text   string  `json:"text,omitempty"`
		Images []Image `json:"images,omitempty"`
	} `json:"content"`
}

func (m *Message) GetText() string {
	return m.Content.Text
}

func (m *Message) GetAuthorName() string {
	if m.Author.Name != "" {
		return m.Author.Name
	}
	return "Unknown"
}

// Image представляет изображение в сообщении.
type Image struct {
	Sizes struct {
		Origin struct {
			URL string `json:"url"`
		} `json:"origin"`
		S640x480 struct {
			URL string `json:"url"`
		} `json:"640x480"`
	} `json:"sizes"`
}

// SendMessageRequest тело запроса для отправки сообщения.
type SendMessageRequest struct {
	Message SendMessage `json:"message"`
}

// SendMessage сообщение для отправки.
type SendMessage struct {
	Text           string `json:"text,omitempty"`
	AttachmentType string `json:"attachment_type,omitempty"`
	URL            string `json:"url,omitempty"`
}

// ChatsResponse представляет ответ API списка чатов.
type ChatsResponse struct {
	Chats []Chat `json:"chats"`
}

// MessagesResponse представляет ответ API списка сообщений.
type MessagesResponse []Message

// Chat представляет чат из списка чатов.
type Chat struct {
	ID          string   `json:"id"`
	Created     int64    `json:"created"`
	Updated     int64    `json:"updated"`
	Context     Context  `json:"context"`
	Users       []User   `json:"users"`
	LastMessage *Message `json:"last_message,omitempty"`
}

// Context представляет контекст чата.
type Context struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}

// User представляет пользователя в чате Avito.
type User struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// SubscriptionsResponse представляет ответ API подписок.
type SubscriptionsResponse struct {
	Subscriptions []Subscription `json:"subscriptions"`
}

// Subscription представляет webhook подписку.
type Subscription struct {
	URL string `json:"url"`
}

// UnsubscribeRequest представляет запрос на отписку от webhook.
type UnsubscribeRequest struct {
	URL string `json:"url"`
}
