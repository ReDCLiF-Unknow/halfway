package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"halfway/internal/discord"
)

// deviceLink asks for a one-time sign-in link, as the profile dialog does.
func (e *env) deviceLink(token string) (link, qr string) {
	e.t.Helper()
	resp, body := e.fetch("POST", "/me/device-link", token, nil)
	var out struct {
		Link, QR string
		Minutes  int
	}
	json.Unmarshal(body, &out)
	if resp.StatusCode != 200 || out.Minutes != 10 {
		e.t.Fatalf("device link: %d %s", resp.StatusCode, body)
	}
	return out.Link, out.QR
}

func TestSigningInOnAnotherDevice(t *testing.T) {
	e := newEnv(t)
	anna := e.register("Anna")
	p := e.create(anna, dinner(""))
	link, qr := e.deviceLink(anna)
	if !strings.HasPrefix(qr, "<svg") || !strings.Contains(qr, `fill="#000"`) {
		t.Errorf("the QR code is not an SVG: %.60s", qr)
	}
	path := strings.TrimPrefix(link, e.srv.URL)
	if !strings.HasPrefix(path, "/d/") {
		t.Fatalf("link %q", link)
	}

	// Opening it asks first, and asking does not spend it (a chat app's
	// preview of the link opens it too).
	for i := 0; i < 2; i++ {
		code, body := e.page("", path)
		if code != 200 || !strings.Contains(body, "Use Halfway here as Anna?") {
			t.Fatalf("opening the link: %d", code)
		}
	}
	resp := e.form("", path, nil)
	phone := cookieFrom(resp)
	if resp.StatusCode != http.StatusSeeOther || phone == "" || phone == anna {
		t.Fatalf("using the link: %d, a key of its own? %v", resp.StatusCode, phone != anna)
	}
	if _, body := e.page(phone, "/polls/"+itoa(p.ID)); !strings.Contains(body, "Friday dinner") {
		t.Error("the new device does not see Anna's poll")
	}
	// Once only.
	if code := e.form("", path, nil).StatusCode; code != http.StatusNotFound {
		t.Errorf("using the link twice: %d", code)
	}
	// Both devices list it; the one using it says so.
	_, body := e.page(phone, "/")
	if !strings.Contains(body, "Signed in with a code") || !strings.Contains(body, "This one") {
		t.Error("the phone's profile does not list it as this device")
	}
	if _, body := e.page(anna, "/"); strings.Contains(body, "This one") || !strings.Contains(body, "Signed in with a code") {
		t.Error("Anna's first device should list the phone, not as itself")
	}

	// Signing it out from the first device stops its key working.
	devices, _ := e.st.Devices(1)
	e.form(anna, "/me/devices/"+itoa(devices[0].ID)+"/remove", nil)
	if code, _ := e.page(phone, "/"); code != http.StatusSeeOther {
		t.Errorf("a signed-out device still gets in: %d", code)
	}
}

func TestDeviceLinksExpireAndWarnBeforeSwitching(t *testing.T) {
	e := newEnv(t)
	anna, ben := e.register("Anna"), e.register("Ben")
	link, _ := e.deviceLink(anna)
	path := strings.TrimPrefix(link, e.srv.URL)

	// Ben's browser opening Anna's link is warned, and Ben's key has never been saved.
	_, body := e.page(ben, path)
	if !strings.Contains(body, "This browser is signed in as Ben now") || !strings.Contains(body, "never been saved") {
		t.Error("switching people should be warned about")
	}
	// Anna opening her own link just goes home.
	if code, _ := e.page(anna, path); code != http.StatusSeeOther {
		t.Errorf("opening your own link: %d", code)
	}

	e.app.now = func() time.Time { return monday.Add(11 * time.Minute) }
	if code, body := e.page("", path); code != http.StatusNotFound || !strings.Contains(body, "work any more") {
		t.Errorf("an expired link: %d", code)
	}
	if code := e.form("", path, nil).StatusCode; code != http.StatusNotFound {
		t.Errorf("using an expired link: %d", code)
	}
	if code, _ := e.page("", "/d/made-up"); code != http.StatusNotFound {
		t.Errorf("a made-up link: %d", code)
	}
}

func TestADeviceCanSignItselfOut(t *testing.T) {
	e := newEnv(t)
	anna := e.register("Anna")
	link, _ := e.deviceLink(anna)
	phone := cookieFrom(e.form("", strings.TrimPrefix(link, e.srv.URL), nil))
	devices, _ := e.st.Devices(1)
	resp := e.form(phone, "/me/devices/"+itoa(devices[0].ID)+"/remove", nil)
	if resp.Header.Get("Location") != "/welcome" {
		t.Errorf("signing this device out went to %q", resp.Header.Get("Location"))
	}
	cleared := false
	for _, c := range resp.Cookies() {
		cleared = cleared || (c.Name == cookieName && c.MaxAge < 0)
	}
	if !cleared {
		t.Error("the cookie was not cleared")
	}
	if code, _ := e.page(anna, "/"); code != 200 {
		t.Error("signing the phone out signed Anna's first device out too")
	}
}

func TestDeviceNames(t *testing.T) {
	for ua, want := range map[string]string{
		"Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0 Mobile Safari/537.36":     "Chrome on Android",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 Version/18.0 Mobile Safari/604.1": "Safari on iPhone",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/129.0 Safari/537.36 Edg/129.0":            "Edge on Windows",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 14.6; rv:131.0) Gecko/20100101 Firefox/131.0":                          "Firefox on a Mac",
		"curl/8.0": "A browser",
	} {
		r, _ := http.NewRequest("GET", "/", nil)
		r.Header.Set("User-Agent", ua)
		if got := deviceName(r); got != want {
			t.Errorf("%q: got %q, want %q", ua, got, want)
		}
	}
}

func TestConnectingADiscordChannel(t *testing.T) {
	hooks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/webhooks/1/ok" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(`{"name": "Dinner club"}`))
	}))
	defer hooks.Close()
	e := newEnv(t)
	e.app.discord = discord.NewWith(hooks.Client(), func(u *url.URL) bool { return strings.HasPrefix(u.String(), hooks.URL+"/api/webhooks/") })
	anna, ben := e.register("Anna"), e.register("Ben")
	p := e.create(anna, dinner(""))
	e.join(ben, p)
	id := itoa(p.ID)
	add := func(token, hook string) int {
		return e.form(token, "/polls/"+id+"/chats/discord", url.Values{"webhook": {hook}}).StatusCode
	}
	want(t, "a participant connecting a channel", add(ben, hooks.URL+"/api/webhooks/1/ok"), http.StatusForbidden)
	want(t, "something that is not a webhook", add(anna, "https://evil.example/x"), http.StatusBadRequest)
	want(t, "a deleted webhook", add(anna, hooks.URL+"/api/webhooks/2/gone"), http.StatusBadRequest)
	want(t, "a working webhook", add(anna, hooks.URL+"/api/webhooks/1/ok"), http.StatusSeeOther)
	chats, _ := e.st.Chats(p.ID)
	if len(chats) != 1 || chats[0].Platform != "discord" || chats[0].Title != "Discord: Dinner club" {
		t.Fatalf("chats %+v", chats)
	}
	_, body := e.page(anna, "/polls/"+id)
	if !strings.Contains(body, "Discord: Dinner club") || strings.Contains(body, "/api/webhooks/1/ok") {
		t.Error("the channel should be listed, and its secret URL never shown")
	}
	e.app.discord = nil
	want(t, "connecting with Discord off", add(anna, hooks.URL+"/api/webhooks/1/ok"), http.StatusNotFound)
}
