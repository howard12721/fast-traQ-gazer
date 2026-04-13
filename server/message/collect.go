package message

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
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
	dmCache   dmChannelCache
}

type dmChannelCache struct {
	mu          sync.RWMutex
	ids         map[string]struct{}
	knownIDs    map[string]struct{}
	initialized bool
}

type wsEvent struct {
	Type string          `json:"type"`
	Body json.RawMessage `json:"body"`
}

type messageCreatedEventBody struct {
	ID       string `json:"id"`
	IsCiting bool   `json:"is_citing"`
}

type channelPathResponse struct {
	Path string `json:"path"`
}

func NewMessageReceiver() *MessageReceiver {
	return &MessageReceiver{
		processor: &messageProcessor{
			queue: make(chan *traq.Message),
		},
		dmCache: dmChannelCache{
			ids:      make(map[string]struct{}),
			knownIDs: make(map[string]struct{}),
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

	isDM, err := m.isDMChannel(message.ChannelId)
	if err != nil {
		return err
	}
	if isDM {
		slog.Info(fmt.Sprintf("Skip DM channel message: %s", message.Id))
		return nil
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

func (m *MessageReceiver) isDMChannel(channelID string) (bool, error) {
	if channelID == "" {
		return false, nil
	}

	if !m.dmCache.isInitialized() {
		dmChannelIDs, knownChannelIDs, err := fetchChannelIDs()
		if err != nil {
			return false, err
		}
		m.dmCache.seed(dmChannelIDs, knownChannelIDs)
	}

	if m.dmCache.contains(channelID) {
		return true, nil
	}

	if m.dmCache.knows(channelID) {
		return false, nil
	}

	isDM, err := fetchIsDMChannel(channelID)
	if err != nil {
		return false, err
	}

	m.dmCache.mark(channelID, isDM)
	return isDM, nil
}

func fetchChannelIDs() (map[string]struct{}, map[string]struct{}, error) {
	client := traq.NewAPIClient(traq.NewConfiguration())
	auth := context.WithValue(context.Background(), traq.ContextAccessToken, repo.UserAccessToken)

	channelList, _, err := client.ChannelApi.GetChannels(auth).IncludeDm(true).Execute()
	if err != nil {
		return nil, nil, err
	}

	dmChannelIDs := make(map[string]struct{}, len(channelList.GetDm()))
	for _, dmChannel := range channelList.GetDm() {
		dmChannelIDs[dmChannel.GetId()] = struct{}{}
	}

	knownChannelIDs := make(map[string]struct{}, len(channelList.GetPublic())+len(channelList.GetDm()))
	for _, channel := range channelList.GetPublic() {
		knownChannelIDs[channel.GetId()] = struct{}{}
	}
	for _, dmChannel := range channelList.GetDm() {
		knownChannelIDs[dmChannel.GetId()] = struct{}{}
	}

	return dmChannelIDs, knownChannelIDs, nil
}

func fetchIsDMChannel(channelID string) (bool, error) {
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("https://q.trap.jp/api/v3/channels/%s/path", channelID), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", authorizationScheme+" "+repo.UserAccessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("failed to fetch channel path: status=%d", resp.StatusCode)
	}

	var channelPath channelPathResponse
	if err := json.NewDecoder(resp.Body).Decode(&channelPath); err != nil {
		return false, err
	}

	return channelPath.Path == "", nil
}

func (c *dmChannelCache) contains(channelID string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	_, ok := c.ids[channelID]
	return ok
}

func (c *dmChannelCache) knows(channelID string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	_, ok := c.knownIDs[channelID]
	return ok
}

func (c *dmChannelCache) isInitialized() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.initialized
}

func (c *dmChannelCache) seed(ids map[string]struct{}, knownIDs map[string]struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.ids = ids
	c.knownIDs = knownIDs
	c.initialized = true
}

func (c *dmChannelCache) mark(channelID string, isDM bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.knownIDs[channelID] = struct{}{}
	if isDM {
		c.ids[channelID] = struct{}{}
	}
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
