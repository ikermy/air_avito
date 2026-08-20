package metrics

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	registerOnce sync.Once

	MessagesReceived          = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "air", Subsystem: "avitobot", Name: "messages_received_total", Help: "Total number of incoming app messages received by the service."}, []string{"bot_id", "message_type"})
	MessagesIgnored           = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "air", Subsystem: "avitobot", Name: "messages_ignored_total", Help: "Total number of incoming app messages ignored by reason."}, []string{"bot_id", "reason"})
	MessagesProcessed         = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "air", Subsystem: "avitobot", Name: "messages_processed_total", Help: "Total number of app messages processed by status."}, []string{"bot_id", "status"})
	CRMRequests               = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "air", Subsystem: "avitobot", Name: "crm_requests_total", Help: "Total number of CRM requests by direction and status."}, []string{"bot_id", "direction", "status"})
	CRMRequestDuration        = prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: "air", Subsystem: "avitobot", Name: "crm_request_duration_seconds", Help: "Duration of CRM requests in seconds.", Buckets: prometheus.DefBuckets}, []string{"bot_id", "direction"})
	AppSend                   = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "air", Subsystem: "avitobot", Name: "app_send_total", Help: "Total number of outgoing app sends by status."}, []string{"bot_id", "status"})
	AppSendDuration           = prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: "air", Subsystem: "avitobot", Name: "app_send_duration_seconds", Help: "Duration of outgoing app sends in seconds.", Buckets: prometheus.DefBuckets}, []string{"bot_id"})
	MessageProcessingDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: "air", Subsystem: "avitobot", Name: "message_processing_duration_seconds", Help: "Duration of app message processing in seconds.", Buckets: prometheus.DefBuckets}, []string{"bot_id", "stage"})
	UserChannelInitDuration   = prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: "air", Subsystem: "avitobot", Name: "user_channel_init_duration_seconds", Help: "Duration of user channel initialization in seconds.", Buckets: prometheus.DefBuckets}, []string{"bot_id", "status"})
	DecryptErrors             = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "air", Subsystem: "avitobot", Name: "decrypt_errors_total", Help: "Total number of decrypt or session-related errors."}, []string{"bot_id", "category"})
	Reconnects                = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "air", Subsystem: "avitobot", Name: "reconnects_total", Help: "Total number of connect and reconnect attempts by result."}, []string{"bot_id", "result"})
	HTTPSRequests             = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "air", Subsystem: "avitobot", Name: "http_requests_total", Help: "Total number of HTTP requests handled by the service."}, []string{"method", "route", "status"})
	HTTPRequestDuration       = prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: "air", Subsystem: "avitobot", Name: "http_request_duration_seconds", Help: "Duration of HTTP requests handled by the service.", Buckets: prometheus.DefBuckets}, []string{"method", "route"})
	ActiveSessions            = prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: "air", Subsystem: "avitobot", Name: "active_sessions", Help: "Current number of active app sessions by state."}, []string{"bot_id", "state"})
	ActiveDialogs             = prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: "air", Subsystem: "avitobot", Name: "active_dialogs", Help: "Current number of active dialogs tracked in memory."}, []string{"bot_id"})
	OperatorModeDialogs       = prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: "air", Subsystem: "avitobot", Name: "operator_mode_dialogs", Help: "Current number of dialogs in operator mode."}, []string{"bot_id"})
)

func Register() {
	registerOnce.Do(func() {
		prometheus.DefaultRegisterer = prometheus.WrapRegistererWithPrefix("", prometheus.DefaultRegisterer)
		registerCollector(collectors.NewGoCollector())
		registerCollector(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
		registerCollector(MessagesReceived)
		registerCollector(MessagesIgnored)
		registerCollector(MessagesProcessed)
		registerCollector(CRMRequests)
		registerCollector(CRMRequestDuration)
		registerCollector(AppSend)
		registerCollector(AppSendDuration)
		registerCollector(MessageProcessingDuration)
		registerCollector(UserChannelInitDuration)
		registerCollector(DecryptErrors)
		registerCollector(Reconnects)
		registerCollector(HTTPSRequests)
		registerCollector(HTTPRequestDuration)
		registerCollector(ActiveSessions)
		registerCollector(ActiveDialogs)
		registerCollector(OperatorModeDialogs)
	})
}

func registerCollector(collector prometheus.Collector) {
	if err := prometheus.Register(collector); err != nil {
		var alreadyRegisteredError prometheus.AlreadyRegisteredError
		if errors.As(err, &alreadyRegisteredError) {
			return
		}
		panic(err)
	}
}

func Handler() http.Handler {
	Register()
	return promhttp.Handler()
}

func ObserveDuration(observer prometheus.Observer, startedAt time.Time) {
	observer.Observe(time.Since(startedAt).Seconds())
}

func NormalizeRoute(path string) string {
	if path == "" {
		return "unknown"
	}
	if path == "/metrics" {
		return path
	}
	trimmed := strings.TrimSuffix(path, "/")
	if trimmed == "" {
		return "/"
	}
	return trimmed
}

func BotLabel(userID uint32) string {
	return strconv.FormatUint(uint64(userID), 10)
}

func TrackActiveDialogs(userID uint32, count int) {
	ActiveDialogs.WithLabelValues(BotLabel(userID)).Set(float64(count))
}

func TrackOperatorModeDialogs(userID uint32, count int) {
	OperatorModeDialogs.WithLabelValues(BotLabel(userID)).Set(float64(count))
}

func IncMessagesReceived(userID uint32, messageType string) {
	MessagesReceived.WithLabelValues(BotLabel(userID), messageType).Inc()
}

func IncMessagesIgnored(userID uint32, reason string) {
	MessagesIgnored.WithLabelValues(BotLabel(userID), reason).Inc()
}

func IncMessagesProcessed(userID uint32, status string) {
	MessagesProcessed.WithLabelValues(BotLabel(userID), status).Inc()
}

func ObserveCRMRequest(userID uint32, direction, status string, startedAt time.Time) {
	botID := BotLabel(userID)
	CRMRequests.WithLabelValues(botID, direction, status).Inc()
	CRMRequestDuration.WithLabelValues(botID, direction).Observe(time.Since(startedAt).Seconds())
}

func ObserveAppSend(userID uint32, status string, startedAt time.Time) {
	botID := BotLabel(userID)
	AppSend.WithLabelValues(botID, status).Inc()
	AppSendDuration.WithLabelValues(botID).Observe(time.Since(startedAt).Seconds())
}

func ObserveMessageProcessing(userID uint32, stage string, startedAt time.Time) {
	MessageProcessingDuration.WithLabelValues(BotLabel(userID), stage).Observe(time.Since(startedAt).Seconds())
}

func ObserveUserChannelInit(userID uint32, status string, startedAt time.Time) {
	UserChannelInitDuration.WithLabelValues(BotLabel(userID), status).Observe(time.Since(startedAt).Seconds())
}

func IncDecryptErrors(userID uint32, category string) {
	DecryptErrors.WithLabelValues(BotLabel(userID), category).Inc()
}

func IncReconnects(userID uint32, result string) {
	Reconnects.WithLabelValues(BotLabel(userID), result).Inc()
}

func SetActiveSessions(userID uint32, state string, count float64) {
	ActiveSessions.WithLabelValues(BotLabel(userID), state).Set(count)
}
