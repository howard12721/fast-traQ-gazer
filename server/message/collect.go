package message

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"traQ-gazer/model"
	"traQ-gazer/repo"

	"github.com/traPtitech/go-traq"
	"golang.org/x/net/websocket"
	"golang.org/x/exp/slog"
)

const (
	traqWebSocketURL    = "wss://q.trap.jp/api/v3/ws"
	traqReconnectWait   = 5 * time.Second
	authorizationScheme = "Bearer"
)

type MessageReceiver struct {
	processor *messageProcessor
}

type wsEvent struct {
	Type string          `json:"type"`
	Body json.RawMessage `json:"body"`
}

type messageCreatedEventBody struct {
	ID string `json:"id"`
}

func NewMessageReceiver() *MessageReceiver {
	return &MessageReceiver{
		processor: &messageProcessor{
			queue: make(chan *traq.Message),
		},
	}
}

// go routineの中で呼ぶこと
func (m *MessageReceiver) Run() {
	if repo.UserAccessToken == "" {
		slog.Info("Skip websocket message receiver because USER_ACCESS_TOKEN is empty")
		return
	}

	go m.processor.run()

	for {
		err := m.listen()
		if err != nil {
			slog.Error(fmt.Sprintf("Failed to receive websocket events: %v", err))
		}
		time.Sleep(traqReconnectWait)
	}
}

func (m *MessageReceiver) listen() error {
	config, err := websocket.NewConfig(traqWebSocketURL, websocketOrigin(traqWebSocketURL))
	if err != nil {
		return err
	}
	if config.Header == nil {
		config.Header = http.Header{}
	}
	config.Header.Set("Authorization", authorizationScheme+" "+repo.UserAccessToken)

	conn, err := websocket.DialConfig(config)
	if err != nil {
		return err
	}
	defer conn.Close()

	slog.Info("Connected to traQ websocket")

	for {
		var payload []byte
		if err := websocket.Message.Receive(conn, &payload); err != nil {
			return err
		}

		if err := m.handleEvent(payload); err != nil {
			slog.Error(fmt.Sprintf("Failed to handle websocket event: %v", err))
		}
	}
}

func (m *MessageReceiver) handleEvent(payload []byte) error {
	var event wsEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return err
	}

	if event.Type != "MESSAGE_CREATED" {
		return nil
	}

	var body messageCreatedEventBody
	if err := json.Unmarshal(event.Body, &body); err != nil {
		return err
	}

	messageID := body.ID
	if messageID == "" {
		return fmt.Errorf("message id is missing in MESSAGE_CREATED event")
	}

	message, err := fetchMessage(messageID)
	if err != nil {
		return err
	}

	m.processor.enqueue(message)
	return nil
}

func websocketOrigin(wsURL string) string {
	parsed, err := url.Parse(wsURL)
	if err != nil {
		return "https://q.trap.jp"
	}

	switch parsed.Scheme {
	case "ws":
		parsed.Scheme = "http"
	case "wss":
		parsed.Scheme = "https"
	}
	parsed.Path = ""
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""

	return parsed.String()
}

func fetchMessage(messageID string) (*traq.Message, error) {
	client := traq.NewAPIClient(traq.NewConfiguration())
	auth := context.WithValue(context.Background(), traq.ContextAccessToken, repo.UserAccessToken)

	message, _, err := client.MessageApi.GetMessage(auth, messageID).Execute()
	if err != nil {
		return nil, err
	}

	return message, nil
}

// 通知メッセージの検索と通知処理のjobを処理する
type messageProcessor struct {
	queue chan *traq.Message
}

// go routineの中で呼ぶ
func (m *messageProcessor) run() {
	for message := range m.queue {
		m.process(*message)
	}
}

func (m *messageProcessor) enqueue(message *traq.Message) {
	m.queue <- message
}

func (m *messageProcessor) process(message traq.Message) {
	messageList, err := convertMessageHits([]traq.Message{message})
	if err != nil {
		slog.Error(fmt.Sprintf("Failed to convert messages: %v", err))
		return
	}
	notifyInfoList, err := findMatchingWords(messageList)
	if err != nil {
		slog.Error(fmt.Sprintf("Failed to process messages: %v", err))
		return
	}

	slog.Info(fmt.Sprintf("Sending %d DMs...", len(notifyInfoList)))

	for _, notifyInfo := range notifyInfoList {
		err := sendMessage(notifyInfo.NotifyTargetTraqUuid, genNotifyMessageContent(notifyInfo.MessageId, notifyInfo.Words...))
		if err != nil {
			slog.Error(fmt.Sprintf("Failed to send message: %v", err))
			continue
		}
	}

	slog.Info("End of send DMs")
}

func genNotifyMessageContent(citeMessageId string, words ...string) string {
	list := make([]string, 0)
	for _, word := range words {
		item := fmt.Sprintf("「%s」", word)
		list = append(list, item)
	}

	return fmt.Sprintf("%s\n https://q.trap.jp/messages/%s", strings.Join(list, ""), citeMessageId)
}

func sendMessage(notifyTargetTraqUUID string, messageContent string) error {
	if repo.AccessToken == "" {
		slog.Info("Skip sendMessage")
		return nil
	}

	client := traq.NewAPIClient(traq.NewConfiguration())
	auth := context.WithValue(context.Background(), traq.ContextAccessToken, repo.AccessToken)
	_, _, err := client.UserApi.PostDirectMessage(auth, notifyTargetTraqUUID).PostMessageRequest(traq.PostMessageRequest{
		Content: messageContent,
	}).Execute()
	if err != nil {
		slog.Info("Error sending message: %v", err)
		return err
	}
	return nil
}

func convertMessageHits(messages []traq.Message) (model.MessageList, error) {
	messageList := model.MessageList{}
	for _, message := range messages {
		messageList = append(messageList, model.MessageItem{
			Id:       message.Id,
			TraqUuid: message.UserId,
			Content:  message.Content,
		})
	}
	return messageList, nil
}
