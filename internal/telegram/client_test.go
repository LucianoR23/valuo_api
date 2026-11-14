package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testToken = "999:secret-token"

func TestClientCalls(t *testing.T) {
	var got []map[string]any
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.HasPrefix(r.URL.Path, "/file/") {
			_, _ = w.Write([]byte("0123456789"))
			return
		}
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		got = append(got, p)
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":7}}`))
		case strings.HasSuffix(r.URL.Path, "/editMessageText"):
			_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: message is not modified"}`))
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"file_path":"voice/a.oga"}}`))
		case strings.HasSuffix(r.URL.Path, "/answerCallbackQuery"):
			_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: query is too old"}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		}
	}))
	defer srv.Close()
	c := NewClient(testToken, srv.URL, nil)
	ctx := context.Background()

	id, err := c.SendMessage(ctx, 5, "hola", Keyboard{{{Text: "✅", CallbackData: "ok:x"}}})
	if err != nil || id != 7 || paths[0] != "/bot"+testToken+"/sendMessage" {
		t.Fatalf("send %d %v %v", id, err, paths)
	}
	if got[0]["chat_id"] != float64(5) || got[0]["parse_mode"] != nil {
		t.Errorf("send params %+v", got[0])
	}
	if err := c.EditMessage(ctx, 5, 7, "hola", nil); err != nil {
		t.Errorf("not modified is not an error: %v", err)
	}
	if rm := got[1]["reply_markup"].(map[string]any); len(rm["inline_keyboard"].([]any)) != 0 {
		t.Errorf("nil keyboard should remove buttons: %+v", rm)
	}
	err = c.AnswerCallback(ctx, "q", "")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != 400 || strings.Contains(err.Error(), testToken) {
		t.Errorf("api error %v", err)
	}
	path, err := c.GetFile(ctx, "f")
	if err != nil || path != "voice/a.oga" {
		t.Fatalf("getFile %q %v", path, err)
	}
	if data, err := c.Download(ctx, path, 10); err != nil || string(data) != "0123456789" {
		t.Errorf("download %q %v", data, err)
	}
	if _, err := c.Download(ctx, path, 9); err == nil {
		t.Error("too big file accepted")
	}
	if err := c.Setup(ctx, "https://api.test/telegram/webhook", "s3cret", Commands); err != nil {
		t.Fatal(err)
	}
	hook := got[len(got)-2]
	if hook["secret_token"] != "s3cret" || len(hook["allowed_updates"].([]any)) != 2 {
		t.Errorf("setWebhook %+v", hook)
	}
}

func TestClientErrorsHideTheToken(t *testing.T) {
	c := NewClient(testToken, "http://127.0.0.1:1", nil)
	_, err := c.SendMessage(context.Background(), 1, "x", nil)
	if err == nil || strings.Contains(err.Error(), testToken) {
		t.Errorf("err %v", err)
	}
	_, err = c.Download(context.Background(), "p", 1)
	if err == nil || strings.Contains(err.Error(), testToken) {
		t.Errorf("download err %v", err)
	}
}
