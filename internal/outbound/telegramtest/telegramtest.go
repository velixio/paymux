// Package telegramtest stands in for the Telegram Bot API in tests, so no test
// reaches the real service.
package telegramtest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"
)

// SentMessage is one sendMessage call as Telegram received it.
type SentMessage struct {
	ChatID              string
	MessageID           int64
	Text                string
	DisableNotification bool
	ForceReply          bool
}

type update struct {
	id                       int64
	chatID, messageID, reply int64
	text                     string
}

// Fake is an http.RoundTripper that answers Bot API calls from memory.
type Fake struct {
	mu        sync.Mutex
	chatID    int64
	chatType  string
	canDelete bool
	webhook   string
	failures  map[string]int
	updates   []update
	sent      []SentMessage
	deleted   []int64
	calls     map[string]int
	offset    int64
	nextMsg   int64
	nextReply int64
	nextUpd   int64
}

// New returns a Fake with a supergroup chat -100123 where the bot may delete.
func New() *Fake {
	return &Fake{chatID: -100123, chatType: "supergroup", canDelete: true, failures: map[string]int{}, calls: map[string]int{},
		nextMsg: 321, nextReply: 1000, nextUpd: 500}
}

// SetChat sets what getChat answers and the chat messages are sent in.
func (f *Fake) SetChat(chatID int64, chatType string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chatID, f.chatType = chatID, chatType
}

// SetBotCanDelete sets whether the bot is an admin that may delete in a group.
func (f *Fake) SetBotCanDelete(can bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.canDelete = can
}

// SetWebhook sets the bot's webhook. While one is set getUpdates answers 409.
func (f *Fake) SetWebhook(url string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.webhook = url
}

// Fail makes a method answer status until it is cleared with status 0.
func (f *Fake) Fail(method string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if status == 0 {
		delete(f.failures, method)
		return
	}
	f.failures[method] = status
}

// Reply queues a reply in the chat. Empty text means a message without text.
// It returns the reply's message id.
func (f *Fake) Reply(toMessageID int64, text string) int64 {
	f.mu.Lock()
	chatID := f.chatID
	f.mu.Unlock()
	return f.ReplyFromChat(chatID, toMessageID, text)
}

// ReplyFromChat queues a reply in the given chat; toMessageID 0 means an ordinary
// message that replies to nothing.
func (f *Fake) ReplyFromChat(chatID, toMessageID int64, text string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextReply++
	f.nextUpd++
	f.updates = append(f.updates, update{id: f.nextUpd, chatID: chatID, messageID: f.nextReply, reply: toMessageID, text: text})
	return f.nextReply
}

// Sent returns every message sent so far.
func (f *Fake) Sent() []SentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SentMessage(nil), f.sent...)
}

// Deleted returns the id of every message deleted so far.
func (f *Fake) Deleted() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.deleted...)
}

// Calls counts the calls of a method.
func (f *Fake) Calls(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

// LastOffset is the offset of the latest getUpdates call.
func (f *Fake) LastOffset() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.offset
}

// RoundTrip answers one Bot API call.
func (f *Fake) RoundTrip(r *http.Request) (*http.Response, error) {
	var body map[string]any
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
	}
	method := path.Base(r.URL.Path)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[method]++
	if status, failing := f.failures[method]; failing {
		return respond(status, fmt.Sprintf(`{"ok":false,"error_code":%d,"description":"failing on purpose"}`, status)), nil
	}
	switch method {
	case "sendMessage":
		f.nextMsg++
		m := SentMessage{ChatID: fmt.Sprint(body["chat_id"]), MessageID: f.nextMsg}
		m.Text, _ = body["text"].(string)
		m.DisableNotification, _ = body["disable_notification"].(bool)
		if markup, ok := body["reply_markup"].(map[string]any); ok {
			m.ForceReply, _ = markup["force_reply"].(bool)
		}
		f.sent = append(f.sent, m)
		return ok(fmt.Sprintf(`{"message_id":%d,"chat":{"id":%d}}`, m.MessageID, f.chatID)), nil
	case "deleteMessage":
		id, _ := body["message_id"].(float64)
		f.deleted = append(f.deleted, int64(id))
		return ok(`true`), nil
	case "getWebhookInfo":
		return ok(fmt.Sprintf(`{"url":%q}`, f.webhook)), nil
	case "getChat":
		return ok(fmt.Sprintf(`{"id":%d,"type":%q}`, f.chatID, f.chatType)), nil
	case "getMe":
		return ok(`{"id":99,"is_bot":true}`), nil
	case "getChatMember":
		if f.canDelete {
			return ok(`{"status":"administrator","can_delete_messages":true}`), nil
		}
		return ok(`{"status":"member"}`), nil
	case "getUpdates":
		if f.webhook != "" {
			return respond(http.StatusConflict, `{"ok":false,"error_code":409,"description":"can't use getUpdates method while webhook is active"}`), nil
		}
		offset, _ := body["offset"].(float64)
		f.offset = int64(offset)
		limit, hasLimit := body["limit"].(float64)
		var out []string
		for _, u := range f.updates {
			if hasLimit && len(out) >= int(limit) {
				break
			}
			if u.id >= f.offset {
				out = append(out, u.json())
			}
		}
		return ok("[" + strings.Join(out, ",") + "]"), nil
	}
	return ok(`{"message_id":1}`), nil
}

func (u update) json() string {
	m := map[string]any{"message_id": u.messageID, "chat": map[string]any{"id": u.chatID}}
	if u.text != "" {
		m["text"] = u.text
	}
	if u.reply != 0 {
		m["reply_to_message"] = map[string]any{"message_id": u.reply}
	}
	raw, _ := json.Marshal(map[string]any{"update_id": u.id, "message": m})
	return string(raw)
}

func ok(result string) *http.Response {
	return respond(http.StatusOK, `{"ok":true,"result":`+result+`}`)
}

func respond(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
