// Package telegramtest stands in for the Telegram Bot API in tests, so no test
// reaches the real service.
package telegramtest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
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
	before    map[string]func(*http.Request)
	lose      map[string]bool
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

// Before runs fn when a method is called, before it is answered. Nil clears it.
func (f *Fake) Before(method string, fn func(*http.Request)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.before == nil {
		f.before = map[string]func(*http.Request){}
	}
	f.before[method] = fn
}

// LoseAnswer makes a method do its work but fail to answer, as when the
// connection drops after Telegram accepted the call.
func (f *Fake) LoseAnswer(method string, lose bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lose == nil {
		f.lose = map[string]bool{}
	}
	f.lose[method] = lose
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
	before := f.before[method]
	f.mu.Unlock()
	if before != nil {
		before(r)
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[method]++
	if status, failing := f.failures[method]; failing {
		return respond(status, fmt.Sprintf(`{"ok":false,"error_code":%d,"description":"failing on purpose"}`, status)), nil
	}
	resp := f.answer(r, method, body)
	if f.lose[method] {
		return nil, errors.New("telegramtest: the answer was lost")
	}
	return resp, nil
}

func (f *Fake) answer(r *http.Request, method string, body map[string]any) *http.Response {
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
		return ok(fmt.Sprintf(`{"message_id":%d,"chat":{"id":%d}}`, m.MessageID, f.chatID))
	case "deleteMessage":
		id, _ := body["message_id"].(float64)
		f.deleted = append(f.deleted, int64(id))
		return ok(`true`)
	case "getWebhookInfo":
		return ok(fmt.Sprintf(`{"url":%q}`, f.webhook))
	case "getChat":
		return ok(fmt.Sprintf(`{"id":%d,"type":%q}`, f.chatID, f.chatType))
	case "getMe":
		return ok(`{"id":99,"is_bot":true}`)
	case "getChatMember":
		if f.canDelete {
			return ok(`{"status":"administrator","can_delete_messages":true}`)
		}
		return ok(`{"status":"member"}`)
	case "getUpdates":
		if f.webhook != "" {
			return respond(http.StatusConflict, `{"ok":false,"error_code":409,"description":"can't use getUpdates method while webhook is active"}`)
		}
		token, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/bot"), "/")
		botID, _ := strconv.ParseInt(strings.SplitN(token, ":", 2)[0], 10, 64)
		offset, _ := body["offset"].(float64)
		f.offset = int64(offset)
		limit, hasLimit := body["limit"].(float64)
		var out []string
		for _, u := range f.updates {
			if hasLimit && len(out) >= int(limit) {
				break
			}
			if u.id >= f.offset {
				out = append(out, f.updateJSON(u, botID))
			}
		}
		return ok("[" + strings.Join(out, ",") + "]")
	}
	return ok(`{"message_id":1}`)
}

// updateJSON is how Telegram shows an update: a reply carries the message it
// answers, with its sender and text when that is a message the bot sent.
func (f *Fake) updateJSON(u update, botID int64) string {
	m := map[string]any{"message_id": u.messageID, "chat": map[string]any{"id": u.chatID}}
	if u.text != "" {
		m["text"] = u.text
	}
	if u.reply != 0 {
		to := map[string]any{"message_id": u.reply}
		for _, sent := range f.sent {
			// Message ids count per chat.
			if sent.MessageID == u.reply && sent.ChatID == strconv.FormatInt(u.chatID, 10) {
				to["from"] = map[string]any{"id": botID, "is_bot": true}
				to["text"] = sent.Text
			}
		}
		m["reply_to_message"] = to
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
