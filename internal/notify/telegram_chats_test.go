package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Find chats is how most people get a group's ID, so what it offers must be
// an ID that still works. A group upgraded to a supergroup answers to a new
// -100 ID and refuses the old one, and a group the bot was removed from
// refuses every message: before this, both were offered next to the good ones.
func TestRecentChatsOffersOnlyChatsThatStillWork(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botT/getUpdates" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":[
			{"my_chat_member":{"chat":{"id":-4001,"type":"group","title":"Ops"},"new_chat_member":{"status":"member"}}},
			{"message":{"chat":{"id":-4001,"type":"group","title":"Ops"},"text":"/start@argus_bot"}},
			{"my_chat_member":{"chat":{"id":-5002,"type":"group","title":"Old team"},"new_chat_member":{"status":"member"}}},
			{"message":{"chat":{"id":77,"type":"private","first_name":"Dewi","last_name":"S"}}},
			{"message":{"chat":{"id":-4001,"type":"group","title":"Ops"},"migrate_to_chat_id":-1004001}},
			{"message":{"chat":{"id":-1004001,"type":"supergroup","title":"Ops"},"migrate_from_chat_id":-4001}},
			{"my_chat_member":{"chat":{"id":-5002,"type":"group","title":"Old team"},"new_chat_member":{"status":"left"}}},
			{"channel_post":{"chat":{"id":-1009,"type":"channel","title":"Status"}}}
		]}`)
	}))
	defer srv.Close()
	tg := NewTelegramBot("T", nil, srv.Client()).WithAPI(srv.URL)
	got, err := tg.RecentChats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []TelegramChat{
		{ID: "-1009", Title: "Status", Kind: "channel"},
		{ID: "-1004001", Title: "Ops", Kind: "supergroup"},
		{ID: "77", Title: "Dewi S", Kind: "private"},
	}
	if len(got) != len(want) {
		t.Fatalf("chats = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("chat %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// Added back after being removed: offered again.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"result":[
			{"my_chat_member":{"chat":{"id":-6,"type":"group","title":"Back"},"new_chat_member":{"status":"kicked"}}},
			{"my_chat_member":{"chat":{"id":-6,"type":"group","title":"Back"},"new_chat_member":{"status":"member"}}}
		]}`)
	}))
	defer srv2.Close()
	got, err = NewTelegramBot("T", nil, srv2.Client()).WithAPI(srv2.URL).RecentChats(context.Background())
	if err != nil || len(got) != 1 || got[0].ID != "-6" {
		t.Fatalf("re-added chat = %+v, %v", got, err)
	}
}

func TestToPostsToOtherChatsWithTheSameBot(t *testing.T) {
	var chats []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		chats = append(chats, r.Form.Get("chat_id"))
		_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
	}))
	defer srv.Close()
	base := NewTelegramBot("T", []string{"1"}, srv.Client()).WithAPI(srv.URL)
	if err := base.To([]string{"-1002"}).Send(context.Background(), "", "x"); err != nil {
		t.Fatal(err)
	}
	if len(chats) != 1 || chats[0] != "-1002" || base.ChatIDs()[0] != "1" {
		t.Fatalf("posted to %v; the original now has %v", chats, base.ChatIDs())
	}
}
