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
	Buttons             []string // the callback data of inline buttons
}

// Answer is one answerCallbackQuery call.
type Answer struct {
	CallbackID, Text string
}

type update struct {
	id                       int64
	chatID, messageID, reply int64
	fromID                   int64
	text                     string
	callback                 string // the button's data when this is a tap
}

// Fake is an http.RoundTripper that answers Bot API calls from memory.
type Fake struct {
	mu        sync.Mutex
	chatID    int64
	webhook   string
	answers   []Answer
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

// BotUsername is the username getMe answers with.
const BotUsername = "paymux_test_bot"

// New returns a Fake whose Reply and Tap come from the private chat of Telegram
// user 42.
func New() *Fake {
	return &Fake{chatID: 42, failures: map[string]int{}, calls: map[string]int{},
		nextMsg: 321, nextReply: 1000, nextUpd: 500}
}

// SetChat sets the private chat, and so the user, that Reply and Tap come from.
func (f *Fake) SetChat(chatID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chatID = chatID
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

// ReplyFromChat queues a reply in the given chat, sent by the user of that number
// (in a private chat they are the same); toMessageID 0 means an ordinary message
// that replies to nothing.
func (f *Fake) ReplyFromChat(chatID, toMessageID int64, text string) int64 {
	return f.ReplyFrom(chatID, chatID, toMessageID, text)
}

// ReplyFrom queues a message sent by fromID in chatID.
func (f *Fake) ReplyFrom(chatID, fromID, toMessageID int64, text string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextReply++
	f.nextUpd++
	f.updates = append(f.updates, update{id: f.nextUpd, chatID: chatID, fromID: fromID, messageID: f.nextReply, reply: toMessageID, text: text})
	return f.nextReply
}

// Start queues "/start <code>" sent by a user in their private chat with the bot,
// as opening a t.me link does.
func (f *Fake) Start(userID int64, code string) {
	f.ReplyFrom(userID, userID, 0, "/start "+code)
}

// Tap queues a tap on a button of a message in the private chat.
func (f *Fake) Tap(messageID int64, data string) {
	f.mu.Lock()
	chatID := f.chatID
	f.mu.Unlock()
	f.TapFrom(chatID, chatID, messageID, data)
}

// TapFrom queues a tap by fromID on a button of a message in chatID.
func (f *Fake) TapFrom(chatID, fromID, messageID int64, data string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextUpd++
	f.updates = append(f.updates, update{id: f.nextUpd, chatID: chatID, fromID: fromID, messageID: messageID, callback: data})
}

// Answers returns every answer to a tap so far.
func (f *Fake) Answers() []Answer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Answer(nil), f.answers...)
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
			rows, _ := markup["inline_keyboard"].([]any)
			for _, row := range rows {
				buttons, _ := row.([]any)
				for _, b := range buttons {
					button, _ := b.(map[string]any)
					data, _ := button["callback_data"].(string)
					m.Buttons = append(m.Buttons, data)
				}
			}
		}
		f.sent = append(f.sent, m)
		return ok(fmt.Sprintf(`{"message_id":%d,"chat":{"id":%s}}`, m.MessageID, m.ChatID))
	case "answerCallbackQuery":
		a := Answer{}
		a.CallbackID, _ = body["callback_query_id"].(string)
		a.Text, _ = body["text"].(string)
		f.answers = append(f.answers, a)
		return ok(`true`)
	case "deleteMessage":
		id, _ := body["message_id"].(float64)
		f.deleted = append(f.deleted, int64(id))
		return ok(`true`)
	case "getWebhookInfo":
		return ok(fmt.Sprintf(`{"url":%q}`, f.webhook))
	case "getMe":
		return ok(`{"id":99,"is_bot":true,"username":"` + BotUsername + `"}`)
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
// answers, and a tap the message whose button it was, each with its sender and
// text when that is a message the bot sent.
func (f *Fake) updateJSON(u update, botID int64) string {
	chatType := "private"
	if u.chatID < 0 {
		chatType = "supergroup"
	}
	chat := map[string]any{"id": u.chatID, "type": chatType}
	botMessage := func(id int64) map[string]any {
		m := map[string]any{"message_id": id, "chat": chat}
		for _, sent := range f.sent {
			// Message ids count per chat.
			if sent.MessageID == id && sent.ChatID == strconv.FormatInt(u.chatID, 10) {
				m["from"] = map[string]any{"id": botID, "is_bot": true}
				m["text"] = sent.Text
			}
		}
		return m
	}
	from := map[string]any{"id": u.fromID, "is_bot": false}
	if u.callback != "" {
		raw, _ := json.Marshal(map[string]any{"update_id": u.id, "callback_query": map[string]any{
			"id": fmt.Sprintf("cb%d", u.id), "from": from, "message": botMessage(u.messageID), "data": u.callback}})
		return string(raw)
	}
	m := map[string]any{"message_id": u.messageID, "chat": chat, "from": from}
	if u.text != "" {
		m["text"] = u.text
	}
	if u.reply != 0 {
		m["reply_to_message"] = botMessage(u.reply)
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
