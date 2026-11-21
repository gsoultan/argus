package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/notify"
)

/*
Routes for the organization's Telegram bots (telegram_bots.go). Admins and
owners only: a bot's chats are where alerts about the whole organization go.
Every change is in the audit log; a token never comes back out.

	GET    /api/v1/telegram                    bots, each kind's choice, the platform bot if usable
	POST   /api/v1/telegram/bots               add a bot (its token is checked with Telegram first)
	PUT    /api/v1/telegram/bots/{id}          change one; an empty token keeps the saved one
	DELETE /api/v1/telegram/bots/{id}?moveTo=  remove one; what used it moves to moveTo
	POST   /api/v1/telegram/bots/{id}/test     send a test message to its chats
	POST   /api/v1/telegram/check              who a token belongs to, saving nothing
	POST   /api/v1/telegram/chats              chats a bot heard from lately, to pick from
	PUT    /api/v1/telegram/routes/{kind}      which bot and chats a kind of alert uses
	POST   /api/v1/telegram/routes/{kind}/test send a test the way that kind would go
*/

func (a *API) telegramAdmin(w http.ResponseWriter, r *http.Request) (string, bool) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return "", false
	}
	if sess.Role != "admin" && sess.Role != "owner" {
		writeErr(w, http.StatusForbidden, "only an admin or owner can see or change Telegram bots")
		return "", false
	}
	return sess.Email, true
}

func (a *API) telegramErr(w http.ResponseWriter, what string, err error) {
	var invalid ErrInvalidTelegram
	switch {
	case errors.As(err, &invalid):
		writeErr(w, http.StatusBadRequest, invalid.msg)
	case errors.Is(err, ErrNoTelegramBot):
		writeErr(w, http.StatusNotFound, "no such Telegram bot")
	default:
		a.fail(w, what, err)
	}
}

func (a *API) audit(r *http.Request, action, actor, target, detail string) {
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{Action: action, Severity: "notice", ActorEmail: actor,
		Target: target, Detail: detail}); err != nil {
		a.log.Error("audit append failed", "error", err, "action", action)
	}
}

// telegramRouteLabels name each kind of alert as the console does.
var telegramRouteLabels = map[string]string{
	TelegramRouteSignIn:  "Sign-in alerts",
	TelegramRouteServer:  "Server connected",
	TelegramRouteWebsite: "Websites",
	TelegramRouteBackup:  "Backups",
}

// telegramView is everything the Telegram settings show.
type telegramView struct {
	Bots   []TelegramBot   `json:"bots"`
	Routes []TelegramRoute `json:"routes"`
	// Platform is the config file's bot, when this organization may use it.
	Platform *platformBotView `json:"platform,omitempty"`
	// CanSave is false when tokens cannot be kept safe here, with why.
	CanSave    bool   `json:"canSave"`
	CannotSave string `json:"cannotSave,omitempty"`
}

type platformBotView struct {
	Chats  []notify.TelegramChat `json:"chats"`
	UsedBy []string              `json:"usedBy"`
}

func (a *API) telegramView(ctx context.Context) (telegramView, error) {
	v := telegramView{CanSave: true}
	if _, err := a.telegramKey(ctx); err != nil {
		v.CanSave, v.CannotSave = false, err.Error()
	}
	bots, err := a.store.TelegramBots(ctx)
	if err != nil {
		return v, err
	}
	routes, err := a.store.TelegramRoutes(ctx)
	if err != nil {
		return v, err
	}
	v.Bots, v.Routes = bots, routes
	if a.platformTelegramUsable(ctx) {
		p := &platformBotView{Chats: []notify.TelegramChat{}, UsedBy: []string{}}
		for _, id := range a.SignInAlerts.PlatformTelegram().ChatIDs() {
			p.Chats = append(p.Chats, notify.TelegramChat{ID: id, Title: id})
		}
		v.Platform = p
	}

	// What sends through each bot, in words.
	defaultID := ""
	for _, b := range bots {
		if b.Default {
			defaultID = b.ID
		}
	}
	use := func(bot, what string) {
		if bot == BotDefault {
			switch {
			case defaultID != "":
				bot = defaultID
			case v.Platform != nil:
				bot = BotPlatform
			}
		}
		if bot == BotPlatform && v.Platform != nil {
			v.Platform.UsedBy = append(v.Platform.UsedBy, what)
			return
		}
		for i := range v.Bots {
			if v.Bots[i].ID == bot {
				v.Bots[i].UsedBy = append(v.Bots[i].UsedBy, what)
			}
		}
	}
	for _, r := range routes {
		if r.Kind == TelegramRouteBackup && (r.Bot == BotPlatform || (r.Bot == BotDefault && defaultID == "")) {
			continue
		}
		if r.Bot != BotOff {
			use(r.Bot, telegramRouteLabels[r.Kind])
		}
	}
	sites, err := a.store.WebsitesWithOwnTelegram(ctx)
	if err != nil {
		return v, err
	}
	for _, s := range sites {
		use(s.TelegramBot, "Website "+s.Name)
	}
	return v, nil
}

func (a *API) getTelegram(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.telegramAdmin(w, r); !ok {
		return
	}
	v, err := a.telegramView(r.Context())
	if err != nil {
		a.fail(w, "telegram bots", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, v)
}

// checkTelegramToken asks Telegram who token is, for the messages people see.
func (a *API) checkTelegramToken(ctx context.Context, token string) (notify.TelegramBotInfo, error) {
	if !notify.ValidTelegramToken(token) {
		return notify.TelegramBotInfo{}, ErrInvalidTelegram{"that is not a bot token: @BotFather gives one like 123456789:AAH…"}
	}
	t := notify.NewTelegramBot(token, nil, a.telegramHTTP())
	if a.telegramAPI != "" {
		t = t.WithAPI(a.telegramAPI)
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	me, err := t.Me(cctx)
	if err != nil {
		return me, ErrInvalidTelegram{"Telegram did not accept the token: " + err.Error()}
	}
	return me, nil
}

func (a *API) postTelegramCheck(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.telegramAdmin(w, r); !ok {
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	me, err := a.checkTelegramToken(r.Context(), strings.TrimSpace(in.Token))
	if err != nil {
		a.telegramErr(w, "check telegram token", err)
		return
	}
	writeJSON(w, http.StatusOK, me)
}

func (a *API) postTelegramChats(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.telegramAdmin(w, r); !ok {
		return
	}
	var in struct {
		Token string `json:"token"`
		Bot   string `json:"bot"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var t *notify.Telegram
	if token := strings.TrimSpace(in.Token); token != "" {
		if !notify.ValidTelegramToken(token) {
			writeErr(w, http.StatusBadRequest, "that is not a bot token: @BotFather gives one like 123456789:AAH…")
			return
		}
		t = notify.NewTelegramBot(token, nil, a.telegramHTTP())
		if a.telegramAPI != "" {
			t = t.WithAPI(a.telegramAPI)
		}
	} else {
		var err error
		if t, _, err = a.telegramBotSender(r.Context(), in.Bot); err != nil {
			a.telegramErr(w, "telegram chats", err)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	chats, err := t.RecentChats(ctx)
	if err != nil {
		msg := err.Error()
		if strings.Contains(strings.ToLower(msg), "webhook") {
			msg = "this bot sends its messages to a webhook, so Argus cannot list its chats: type the chat IDs instead"
		}
		writeErr(w, http.StatusBadGateway, msg)
		return
	}
	if chats == nil {
		chats = []notify.TelegramChat{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"chats": chats})
}

// readBotInput checks a bot from the console and, when it carries a token,
// asks Telegram who it is and seals it.
func (a *API) readBotInput(r *http.Request, needToken bool) (in TelegramBotInput, username string, sealed []byte, err error) {
	if err = decode(r, &in); err != nil {
		return in, "", nil, ErrInvalidTelegram{err.Error()}
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Token = strings.TrimSpace(in.Token)
	if l := len([]rune(in.Name)); l == 0 || l > 80 {
		return in, "", nil, ErrInvalidTelegram{"give the bot a name of up to 80 characters"}
	}
	if in.Chats, err = cleanTelegramChats(in.Chats); err != nil {
		return in, "", nil, err
	}
	if in.Token == "" {
		if needToken {
			return in, "", nil, ErrInvalidTelegram{"paste the bot's token from @BotFather"}
		}
		return in, "", nil, nil
	}
	if _, err = a.telegramKey(r.Context()); err != nil {
		return in, "", nil, err
	}
	me, err := a.checkTelegramToken(r.Context(), in.Token)
	if err != nil {
		return in, "", nil, err
	}
	if sealed, err = a.sealTelegramToken(r.Context(), in.Token); err != nil {
		return in, "", nil, err
	}
	return in, me.Username, sealed, nil
}

func chatList(chats []notify.TelegramChat) string {
	if len(chats) == 0 {
		return "no chats"
	}
	names := make([]string, 0, len(chats))
	for _, c := range chats {
		if c.Title != "" && c.Title != c.ID {
			names = append(names, c.Title+" ("+c.ID+")")
		} else {
			names = append(names, c.ID)
		}
	}
	return strings.Join(names, ", ")
}

func (a *API) postTelegramBot(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.telegramAdmin(w, r)
	if !ok {
		return
	}
	in, username, sealed, err := a.readBotInput(r, true)
	if err != nil {
		a.telegramErr(w, "add telegram bot", err)
		return
	}
	b, err := a.store.SaveTelegramBot(r.Context(), "", in, username, tokenTail(in.Token), sealed, actor)
	if err != nil {
		a.telegramErr(w, "add telegram bot", err)
		return
	}
	detail := fmt.Sprintf("Added Telegram bot %s (@%s, token ending %s) posting to %s.", b.Name, b.Username, b.TokenTail, chatList(b.Chats))
	if b.Default {
		detail += " It is the default bot."
	}
	a.audit(r, "telegram.bot.add", actor, "telegram bot "+b.Name, detail)
	writeJSON(w, http.StatusCreated, b)
}

func (a *API) putTelegramBot(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.telegramAdmin(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	before, _, err := a.store.TelegramBot(r.Context(), id)
	if err != nil {
		a.telegramErr(w, "change telegram bot", err)
		return
	}
	in, username, sealed, err := a.readBotInput(r, false)
	if err != nil {
		a.telegramErr(w, "change telegram bot", err)
		return
	}
	b, err := a.store.SaveTelegramBot(r.Context(), id, in, username, tokenTail(in.Token), sealed, actor)
	if err != nil {
		a.telegramErr(w, "change telegram bot", err)
		return
	}
	var said []string
	if before.Name != b.Name {
		said = append(said, "renamed from "+before.Name)
	}
	if sealed != nil {
		said = append(said, "new token ending "+b.TokenTail+" (@"+b.Username+")")
	}
	if chatList(before.Chats) != chatList(b.Chats) {
		said = append(said, "chats now "+chatList(b.Chats))
	}
	if before.Default != b.Default {
		said = append(said, map[bool]string{true: "made the default bot", false: "no longer the default bot"}[b.Default])
	}
	if len(said) == 0 {
		said = []string{"nothing changed"}
	}
	a.audit(r, "telegram.bot.change", actor, "telegram bot "+b.Name, "Changed Telegram bot "+b.Name+": "+strings.Join(said, "; ")+".")
	writeJSON(w, http.StatusOK, b)
}

func (a *API) deleteTelegramBot(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.telegramAdmin(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	b, _, err := a.store.TelegramBot(r.Context(), id)
	if err != nil {
		a.telegramErr(w, "remove telegram bot", err)
		return
	}
	moveTo := r.URL.Query().Get("moveTo")
	if moveTo == id {
		writeErr(w, http.StatusBadRequest, "choose another bot for what used this one")
		return
	}
	if err := a.validRouteBot(r.Context(), moveTo); err != nil {
		a.telegramErr(w, "remove telegram bot", err)
		return
	}
	if err := a.store.DeleteTelegramBot(r.Context(), id, moveTo); err != nil {
		a.telegramErr(w, "remove telegram bot", err)
		return
	}
	a.audit(r, "telegram.bot.remove", actor, "telegram bot "+b.Name,
		"Removed Telegram bot "+b.Name+" (@"+b.Username+"); what used it now uses "+a.botWords(r.Context(), moveTo)+".")
	w.WriteHeader(http.StatusNoContent)
}

// validRouteBot checks a choice of bot names one that can be used here.
func (a *API) validRouteBot(ctx context.Context, bot string) error {
	switch bot {
	case BotDefault, BotOff:
		return nil
	case BotPlatform:
		if !a.platformTelegramUsable(ctx) {
			return ErrInvalidTelegram{"the platform bot is not available to this organization"}
		}
		return nil
	}
	if _, _, err := a.store.TelegramBot(ctx, bot); err != nil {
		if errors.Is(err, ErrNoTelegramBot) {
			return ErrInvalidTelegram{"choose one of the Telegram bots"}
		}
		return err
	}
	return nil
}

// botWords names a choice of bot, for the audit log.
func (a *API) botWords(ctx context.Context, bot string) string {
	switch bot {
	case BotDefault:
		return "the default bot"
	case BotOff:
		return "no Telegram"
	case BotPlatform:
		return "the platform bot"
	}
	if b, _, err := a.store.TelegramBot(ctx, bot); err == nil {
		return "bot " + b.Name
	}
	return "bot " + bot
}

func (a *API) postTelegramBotTest(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.telegramAdmin(w, r)
	if !ok {
		return
	}
	t, why := a.telegramFor(r.Context(), r.PathValue("id"), nil, false)
	if t.bot == nil {
		writeErr(w, http.StatusConflict, strings.ToUpper(why[:1])+why[1:])
		return
	}
	text := telegramText("🧪", "Test from Argus", "Sent by "+actor+" to check that "+t.name+" can post here.")
	if err := a.sendTelegram(r.Context(), t, text, "", ""); err != nil {
		writeErr(w, http.StatusBadGateway, "Telegram did not take it: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bot": t.name, "chats": t.chats})
}

func validRouteKind(kind string) bool {
	_, ok := telegramRouteLabels[kind]
	return ok
}

func (a *API) putTelegramRoute(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.telegramAdmin(w, r)
	if !ok {
		return
	}
	kind := r.PathValue("kind")
	if !validRouteKind(kind) {
		writeErr(w, http.StatusNotFound, "no such kind of alert")
		return
	}
	var in struct {
		Bot   string   `json:"bot"`
		Chats []string `json:"chats"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	in.Bot = strings.TrimSpace(in.Bot)
	in.Chats = cleanChats(in.Chats)
	if in.Bot == BotOff {
		in.Chats = []string{}
	}
	if len(in.Chats) > maxTelegramChats {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("choose at most %d chats", maxTelegramChats))
		return
	}
	if kind == TelegramRouteBackup && in.Bot == BotPlatform {
		writeErr(w, http.StatusBadRequest, "the platform bot already gets backup alerts through the platform's own alert channels")
		return
	}
	if err := a.validRouteBot(r.Context(), in.Bot); err != nil {
		a.telegramErr(w, "telegram route", err)
		return
	}
	before, _ := a.store.TelegramRoute(r.Context(), kind)
	if err := a.store.SetTelegramRoute(r.Context(), TelegramRoute{Kind: kind, Bot: in.Bot, Chats: in.Chats}, actor); err != nil {
		a.fail(w, "telegram route", err)
		return
	}
	say := func(bot string, chats []string) string {
		s := a.botWords(r.Context(), bot)
		if bot != BotOff {
			if len(chats) == 0 {
				s += ", all of its chats"
			} else {
				s += ", to " + strings.Join(chats, ", ")
			}
		}
		return s
	}
	a.audit(r, "telegram.route", actor, "telegram "+kind,
		"Changed where "+telegramRouteLabels[kind]+" go on Telegram from "+say(before.Bot, before.Chats)+" to "+say(in.Bot, in.Chats)+".")
	v, err := a.telegramView(r.Context())
	if err != nil {
		a.fail(w, "telegram bots", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) postTelegramRouteTest(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.telegramAdmin(w, r)
	if !ok {
		return
	}
	kind := r.PathValue("kind")
	if !validRouteKind(kind) {
		writeErr(w, http.StatusNotFound, "no such kind of alert")
		return
	}
	t, why := a.telegramForKind(r.Context(), kind)
	if t.bot == nil {
		writeErr(w, http.StatusConflict, strings.ToUpper(why[:1])+why[1:])
		return
	}
	text := telegramText("🧪", "Test: "+telegramRouteLabels[kind]+" reach this chat",
		"Sent by "+actor+" from Argus. "+telegramRouteLabels[kind]+" are sent here through "+t.name+".")
	if err := a.sendTelegram(r.Context(), t, text, "", ""); err != nil {
		writeErr(w, http.StatusBadGateway, "Telegram did not take it: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bot": t.name, "chats": t.chats})
}
