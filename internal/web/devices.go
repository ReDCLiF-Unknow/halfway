package web

import (
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"rsc.io/qr"

	"halfway/internal/store"
)

// Signing in on another device: the profile makes a one-time link, shown as
// a QR code to scan with a phone, and the device that opens it gets a key of
// its own. Pasting the sign-in key still works too.

// meDeviceLink makes a one-time sign-in link for the caller, as JSON for the
// profile dialog: the link, and the QR code that is the same link.
func (s *Server) meDeviceLink(w http.ResponseWriter, r *http.Request, u *store.User) {
	code, expires, err := s.store.NewDeviceLink(u.ID, s.now())
	if err != nil {
		s.fail(w, err)
		return
	}
	link := baseURL(r) + "/d/" + code
	svg, err := qrSVG(link)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"link": link, "qr": string(svg), "minutes": int(store.DeviceLinkLife.Minutes()), "expires": expires.Unix(),
	})
}

// qrSVG draws text as a QR code: dark squares on a white ground with the
// quiet border scanners need, whatever the page's theme.
func qrSVG(text string) (template.HTML, error) {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}
	const quiet = 4
	n := code.Size + 2*quiet
	var path strings.Builder
	for y := 0; y < code.Size; y++ {
		for x := 0; x < code.Size; x++ {
			if code.Black(x, y) {
				path.WriteString("M" + strconv.Itoa(x+quiet) + " " + strconv.Itoa(y+quiet) + "h1v1h-1z")
			}
		}
	}
	size := strconv.Itoa(n)
	return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ` + size + ` ` + size +
		`" shape-rendering="crispEdges" role="img" aria-label="QR code of the sign-in link">` +
		`<rect width="` + size + `" height="` + size + `" fill="#fff"/><path d="` + path.String() + `" fill="#000"/></svg>`), nil
}

// deviceName says what kind of device a browser is, well enough to tell
// one's own devices apart in a list: "Chrome on Android".
func deviceName(r *http.Request) string {
	ua := r.UserAgent()
	browser := "A browser"
	for _, b := range []struct{ token, name string }{
		{"Edg/", "Edge"}, {"OPR/", "Opera"}, {"Firefox/", "Firefox"}, {"CriOS/", "Chrome"}, {"Chrome/", "Chrome"}, {"Safari/", "Safari"},
	} {
		if strings.Contains(ua, b.token) {
			browser = b.name
			break
		}
	}
	for _, o := range []struct{ token, name string }{
		{"iPhone", "iPhone"}, {"iPad", "iPad"}, {"Android", "Android"}, {"Windows", "Windows"}, {"Mac OS X", "a Mac"}, {"Linux", "Linux"},
	} {
		if strings.Contains(ua, o.token) {
			return browser + " on " + o.name
		}
	}
	return browser
}

func goneDeviceLink() simplePage {
	return simplePage{
		Title:    "Link expired",
		Heading:  "This sign-in link doesn't work any more",
		Subtitle: "Each one works once, for 10 minutes. Make a new one in your profile on the device you're already signed in on.",
	}
}

// deviceGet asks before signing in with a one-time link. Opening a link is a
// GET, which a chat app's link preview makes too; it must not spend the link,
// and only a click on this site does.
func (s *Server) deviceGet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	code := r.PathValue("code")
	owner, err := s.store.PeekDeviceLink(code, s.now())
	if err != nil {
		s.renderTmpl(w, http.StatusNotFound, "welcome.html", goneDeviceLink())
		return
	}
	cur := userFrom(r)
	if cur != nil && cur.ID == owner.ID {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	pg := simplePage{
		Title:    "Sign in",
		Heading:  "Use Halfway here as " + owner.Name + "?",
		Subtitle: "This device gets a key of its own, and sees all of " + owner.Name + "'s polls. You can sign it out again from your profile on any of your devices.",
		Action:   "/d/" + code,
		Next:     "/",
		Button:   "Sign in as " + owner.Name,
	}
	if cur != nil {
		pg.Warning = "This browser is signed in as " + cur.Name + " now. Signing in as " + owner.Name + " replaces that."
		if !cur.TokenSaved && cur.Device == 0 {
			pg.Warning += " " + cur.Name + "'s sign-in key has never been saved, so without it there is no way back. Save it from the profile first."
		}
	}
	s.renderTmpl(w, http.StatusOK, "welcome.html", pg)
}

// devicePost spends a one-time link and signs this browser in with the key
// it gets for it.
func (s *Server) devicePost(w http.ResponseWriter, r *http.Request) {
	if !s.signups.allow(clientIP(r)) {
		w.Header().Set("Retry-After", "60")
		s.renderTmpl(w, http.StatusTooManyRequests, "welcome.html", simplePage{
			Title: "Too many", Heading: "Too many sign-ins from this connection", Subtitle: "Try again in a minute.",
		})
		return
	}
	_, token, err := s.store.UseDeviceLink(r.PathValue("code"), deviceName(r), s.now())
	if errors.Is(err, store.ErrNotFound) {
		s.renderTmpl(w, http.StatusNotFound, "welcome.html", goneDeviceLink())
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	setSession(w, r, token)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// meRemoveDevice signs one of the caller's devices out. Signing out the one
// in use lands on the welcome page.
func (s *Server) meRemoveDevice(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.store.RemoveDevice(u.ID, id); err != nil {
		s.htmlErr(w, r, err)
		return
	}
	if id == u.Device {
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
		http.Redirect(w, r, "/welcome", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, backTo(r, "/"), http.StatusSeeOther)
}
