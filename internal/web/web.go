package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"halfway/internal/places"
	"halfway/internal/store"
	"halfway/internal/telegram"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// manifestJSON makes Halfway installable: "Add to Home Screen" (or "Install"
// in Chrome/Edge) gives it an icon and a window without browser chrome. There
// is no service worker, so it is not usable offline.
const manifestJSON = `{
  "name": "Halfway",
  "short_name": "Halfway",
  "description": "A poll that settles when friends meet, and then decides for them",
  "start_url": "/",
  "scope": "/",
  "display": "standalone",
  "background_color": "#151f2c",
  "theme_color": "#1d273b",
  "icons": [
    {"src": "/static/icon-192.png", "sizes": "192x192", "type": "image/png", "purpose": "any maskable"},
    {"src": "/static/icon-512.png", "sizes": "512x512", "type": "image/png", "purpose": "any maskable"}
  ]
}`

func serveManifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	io.WriteString(w, manifestJSON)
}

const cookieName = "halfway_token"

var avatarColors = []string{"blue", "azure", "indigo", "purple", "pink", "red", "orange", "yellow", "lime", "green", "teal", "cyan"}

// category is how each kind of poll looks.
type category struct{ Name, Icon, Color string }

var categories = map[string]category{
	"coffee": {"Coffee", "coffee", "orange"},
	"dinner": {"Dinner", "tools-kitchen-2", "red"},
	"drinks": {"Drinks", "beer", "yellow"},
	"hike":   {"Hike", "trekking", "green"},
	"other":  {"Meetup", "calendar-event", "blue"},
}

// eventLength is how long each kind of meetup is put in a calendar for.
var eventLength = map[string]time.Duration{
	"coffee": time.Hour, "dinner": 2 * time.Hour, "drinks": 3 * time.Hour, "hike": 4 * time.Hour, "other": 2 * time.Hour,
}

func stampTime(stamp string) (time.Time, bool) {
	t, err := time.ParseInLocation(store.Stamp, stamp, time.Local)
	return t, err == nil
}

func formatStamp(stamp, layout string) string {
	t, ok := stampTime(stamp)
	if !ok {
		return stamp
	}
	return t.Format(layout)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	if word == "person" {
		return strconv.Itoa(n) + " people"
	}
	return strconv.Itoa(n) + " " + word + "s"
}

// until says how long it is until stamp: "in 3 hours", "in 2 days".
func until(stamp string, now time.Time) string {
	t, ok := stampTime(stamp)
	if !ok {
		return ""
	}
	d := t.Sub(now)
	switch {
	case d < 0:
		return "passed"
	case d < time.Minute:
		return "in a moment"
	case d < time.Hour:
		return "in " + plural(int(d.Minutes()), "minute")
	case d < 24*time.Hour:
		return "in " + plural(int(d.Hours()), "hour")
	case d < 14*24*time.Hour:
		return "in " + plural(int(d.Hours()/24), "day")
	}
	return "in " + plural(int(d.Hours()/24/7), "week")
}

var funcs = template.FuncMap{
	// color picks a stable Tabler colour name for a user.
	"color": func(id int64) string { return avatarColors[int(id%int64(len(avatarColors)))] },
	// initial is the first letter of a name, upper-cased, for avatars.
	"initial": func(name string) string {
		r, _ := utf8.DecodeRuneInString(name)
		if r == utf8.RuneError {
			return "?"
		}
		return strings.ToUpper(string(r))
	},
	"cat": func(c string) category {
		if k, ok := categories[c]; ok {
			return k
		}
		return categories["other"]
	},
	"categories": func() []string { return store.Categories },
	// slotTimes are the times of day the new poll form offers in one tap.
	"slotTimes": func() []string {
		return []string{"09:00", "10:00", "12:00", "15:00", "18:00", "19:00", "19:30", "20:00"}
	},
	// label is "Thu 8 Oct, 19:30"; the others are its parts, for date tiles.
	"label":   store.Label,
	"weekday": func(s string) string { return formatStamp(s, "Mon") },
	"dayNum":  func(s string) string { return formatStamp(s, "2") },
	"month":   func(s string) string { return formatStamp(s, "Jan") },
	"clock":   func(s string) string { return formatStamp(s, "15:04") },
	"long":    func(s string) string { return formatStamp(s, "Monday 2 January, 15:04") },
	"until":   until,
	"plural":  plural,
	"percent": func(n, of int) int {
		if of <= 0 {
			return 0
		}
		return min(100, n*100/of)
	},
	"names": func(people []store.Vote) string {
		var n []string
		for _, p := range people {
			n = append(n, p.Name)
		}
		return strings.Join(n, ", ")
	},
	"add":   func(a, b int) int { return a + b },
	"row":   func(d pageData, sl slotRow) slotView { return slotView{slotRow: sl, Page: d} },
	"modes": func() []string { return places.Modes },
	// modeIcon and modeName are how each way of travelling is shown.
	"modeIcon": func(m string) string {
		return map[string]string{"walk": "walk", "bike": "bike", "transit": "bus", "car": "car"}[places.CleanMode(m)]
	},
	"modeName": func(m string) string {
		return map[string]string{"walk": "Walk", "bike": "Bike", "transit": "Transit", "car": "Car"}[places.CleanMode(m)]
	},
	"card": func(d pageData, p store.Summary) cardView {
		return cardView{Summary: p, Now: d.Now, NowStamp: d.NowStamp}
	},
}

// cardView is what the "pollcard" template renders.
type cardView struct {
	store.Summary
	Now      time.Time
	NowStamp string
}

type Server struct {
	store *store.Store
	bot   *telegram.Bot // nil when Telegram is not set up
	// finder looks places up; nil when finding places is turned off.
	finder places.Finder
	tmpl   *template.Template
	mux    *http.ServeMux
	hub    *hub
	// signups limits how fast one address can create identities, the only
	// thing a stranger can do here without a link already.
	signups *limiter
	// searches and suggests keep place lookups to what a person typing would
	// make, as each one is a request to OpenStreetMap.
	searches, suggests *limiter
	// sameOrigin refuses requests that change something when a browser sent
	// them from another site. SameSite=Lax stops such a request carrying the
	// victim's cookie, but not its response setting a new one: without this a
	// page elsewhere could sign you in as someone else, replacing your token.
	sameOrigin *http.CrossOriginProtection
	// kicks wakes the loop that posts decisions to chats, so a decision made
	// by a click is posted at once rather than on the next tick.
	kicks chan struct{}
	// now is the time, which tests move to see a deadline pass.
	now func() time.Time
}

// New makes the web app. bot is nil unless Telegram is set up, and finder
// unless finding places is.
func New(s *store.Store, bot *telegram.Bot, finder places.Finder) *Server {
	srv := &Server{
		store:      s,
		bot:        bot,
		finder:     finder,
		searches:   newLimiter(20, 3*time.Second),
		suggests:   newLimiter(3, 20*time.Second),
		tmpl:       template.Must(template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html")),
		mux:        http.NewServeMux(),
		hub:        newHub(),
		signups:    newLimiter(signupBurst, signupRefill),
		sameOrigin: http.NewCrossOriginProtection(),
		kicks:      make(chan struct{}, 1),
		now:        time.Now,
	}
	s.SetNotifier(srv.hub.publish)
	srv.routes()
	return srv
}

// Run decides polls as their deadlines pass and posts decisions to chats,
// until ctx ends. Nothing else has to be running for a deadline to be kept.
func (s *Server) Run(ctx context.Context) {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		if _, err := s.store.DecideDue(s.now()); err != nil {
			log.Print(err)
		}
		if err := s.store.PurgeStarts(s.now()); err != nil {
			log.Print(err)
		}
		if s.bot != nil {
			s.bot.Deliver(ctx, s.store)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-s.kicks:
		}
	}
}

// kick asks Run to go round again now, as a decision has just been made.
func (s *Server) kick() {
	select {
	case s.kicks <- struct{}{}:
	default:
	}
}

type userKey struct{}

// csp keeps the page to its own origin. The inline scripts and styles need
// 'unsafe-inline', so this does not stop injected script from running; what it
// does stop is a page fetching or sending anything anywhere else, which is
// what an injection would want to do.
const csp = "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; font-src 'self'; connect-src 'self'; form-action 'self'; " +
	"frame-ancestors 'none'; base-uri 'none'"

// CloseStreams ends every live-update stream, for a server that is shutting
// down: register it with http.Server.RegisterOnShutdown. Ordinary requests are
// left to finish.
func (s *Server) CloseStreams() { s.hub.close() }

// ServeHTTP identifies the caller (cookie or bearer token) before routing.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Invite and organizer links sit in the address bar, so no page may pass
	// its address on to wherever a link on it leads.
	w.Header().Set("Referrer-Policy", "no-referrer")
	// What comes back depends on whether the browser takes gzip, so a cache
	// between us must not hand one kind to the other.
	w.Header().Add("Vary", "Accept-Encoding")
	if wantsGzip(r) {
		gw := &gzipResponse{ResponseWriter: w}
		defer gw.finish()
		w = gw
	}
	if err := s.sameOrigin.Check(r); err != nil {
		http.Error(w, "refused: that request came from another site", http.StatusForbidden)
		return
	}
	if token := tokenFrom(r); token != "" {
		if u, err := s.store.UserByToken(token); err == nil {
			r = r.WithContext(context.WithValue(r.Context(), userKey{}, &u))
		}
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	// App icons and the install manifest (public: browsers fetch them without cookies)
	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	files := http.StripPrefix("/static/", http.FileServerFS(static))
	s.mux.HandleFunc("GET /static/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		files.ServeHTTP(w, r)
	})
	s.mux.HandleFunc("GET /manifest.webmanifest", serveManifest)

	// Names, invites and the profile
	s.mux.HandleFunc("GET /welcome", s.welcomeGet)
	s.mux.HandleFunc("POST /welcome", s.limited(s.welcomePost, s.tooManySignups))
	s.mux.HandleFunc("POST /welcome/token", s.welcomeToken)
	s.mux.HandleFunc("GET /i/{code}", s.inviteGet)
	s.mux.HandleFunc("POST /i/{code}", s.invitePost)
	s.mux.HandleFunc("GET /o/{code}", s.organizerGet)
	s.mux.HandleFunc("POST /o/{code}", s.organizerPost)
	s.mux.HandleFunc("GET /me", s.authed(s.meGet))
	s.mux.HandleFunc("GET /me/token", s.authed(s.meToken))
	s.mux.HandleFunc("POST /me/token/saved", s.authed(s.meTokenSaved))
	s.mux.HandleFunc("POST /me", s.authed(s.mePost))

	// Pages
	s.mux.HandleFunc("GET /{$}", s.authed(s.pageDashboard))
	s.mux.HandleFunc("GET /answer", s.authed(s.pageAnswer))
	s.mux.HandleFunc("GET /new", s.authed(s.pageNew))
	s.mux.HandleFunc("POST /polls", s.authed(s.formCreate))
	s.mux.HandleFunc("GET /polls/{id}", s.member(s.pagePoll))
	s.mux.HandleFunc("GET /polls/{id}/event.ics", s.member(s.calendar))
	s.mux.HandleFunc("GET /events", s.authed(s.events))

	// Everyone on a poll
	s.mux.HandleFunc("POST /polls/{id}/slots/{slot}/vote", s.member(s.formVote))
	s.mux.HandleFunc("POST /polls/{id}/leave", s.member(s.formLeave))

	// Where to meet
	s.mux.HandleFunc("GET /polls/{id}/places", s.member(s.placesPoll(s.search)))
	s.mux.HandleFunc("POST /polls/{id}/start", s.member(s.placesPoll(s.formStart)))
	s.mux.HandleFunc("POST /polls/{id}/start/mode", s.member(s.placesPoll(s.formMode)))
	s.mux.HandleFunc("POST /polls/{id}/start/clear", s.member(s.placesPoll(s.formClearStart)))
	s.mux.HandleFunc("POST /polls/{id}/venues/suggest", s.member(s.placesPoll(s.formSuggest)))
	s.mux.HandleFunc("POST /polls/{id}/venues/{venue}/vote", s.member(s.placesPoll(s.formVenueVote)))
	s.mux.HandleFunc("POST /polls/{id}/venues", s.organizer(s.placesPoll(s.formAddVenue)))
	s.mux.HandleFunc("POST /polls/{id}/venues/{venue}/pick", s.organizer(s.placesPoll(s.formVenuePick)))
	s.mux.HandleFunc("POST /polls/{id}/venues/{venue}/remove", s.organizer(s.placesPoll(s.formVenueRemove)))

	// Organizers
	s.mux.HandleFunc("POST /polls/{id}/rename", s.organizer(s.formRename))
	s.mux.HandleFunc("POST /polls/{id}/settings", s.organizer(s.formSettings))
	s.mux.HandleFunc("POST /polls/{id}/decide", s.organizer(s.formDecide))
	s.mux.HandleFunc("POST /polls/{id}/slots/{slot}/pick", s.organizer(s.formPick))
	s.mux.HandleFunc("POST /polls/{id}/invite/reset", s.organizer(s.formInviteReset))
	s.mux.HandleFunc("POST /polls/{id}/invite/close", s.organizer(s.formInviteOpen(false)))
	s.mux.HandleFunc("POST /polls/{id}/invite/open", s.organizer(s.formInviteOpen(true)))
	s.mux.HandleFunc("POST /polls/{id}/organizer/reset", s.organizer(s.formOrganizerReset))
	s.mux.HandleFunc("POST /polls/{id}/people/{user}/remove", s.organizer(s.formRemovePerson))
	s.mux.HandleFunc("POST /polls/{id}/chats/{chat}/remove", s.organizer(s.formRemoveChat))
	s.mux.HandleFunc("POST /polls/{id}/delete", s.organizer(s.formDelete))
	s.mux.HandleFunc("POST /polls/{id}/restore", s.authed(s.formRestore))
}

// ---- identity ---------------------------------------------------------

func tokenFrom(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	if c, err := r.Cookie(cookieName); err == nil {
		return c.Value
	}
	return ""
}

func userFrom(r *http.Request) *store.User {
	u, _ := r.Context().Value(userKey{}).(*store.User)
	return u
}

// isHTTPS reports whether the browser reached us over HTTPS, either directly
// or through a proxy that terminated TLS and said so. Trusting the header is
// safe for both of its uses: forging it can only make a cookie stricter or a
// link more secure than it needed to be, never less.
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func setSession(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   365 * 24 * 3600,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// The token is the whole account, so it must never travel over plain
		// HTTP. Behind a proxy that terminates TLS, r.TLS is always nil, which
		// would leave this off in exactly the deployment it matters most.
		Secure: isHTTPS(r),
	})
}

// safeNext keeps post-login redirects on this site.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, `/\`) {
		return "/"
	}
	return next
}

func baseURL(r *http.Request) string {
	scheme := "http"
	if isHTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func inviteURL(r *http.Request, p store.Poll) string    { return baseURL(r) + "/i/" + p.InviteCode }
func organizerURL(r *http.Request, p store.Poll) string { return baseURL(r) + "/o/" + p.OrganizerCode }
func pollPath(id int64) string                          { return "/polls/" + strconv.FormatInt(id, 10) }

type userHandler func(http.ResponseWriter, *http.Request, *store.User)

// authed sends visitors without a name to the welcome page first.
func (s *Server) authed(h userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := userFrom(r)
		if u == nil {
			dest := "/welcome"
			if r.Method == http.MethodGet {
				dest += "?next=" + url.QueryEscape(r.URL.RequestURI())
			}
			http.Redirect(w, r, dest, http.StatusSeeOther)
			return
		}
		h(w, r, u)
	}
}

// pollCtx is what a handler for one poll is given: the poll, and who is
// asking and whether they organize it.
type pollCtx struct {
	User      *store.User
	Poll      store.Poll
	Organizer bool
}

type pollHandler func(http.ResponseWriter, *http.Request, pollCtx)

// member lets through the people on a poll. Anyone else is told it does not
// exist, which is the same as for a poll that does not.
func (s *Server) member(h pollHandler) http.HandlerFunc {
	return s.authed(func(w http.ResponseWriter, r *http.Request, u *store.User) {
		id, ok := pathID(r, "id")
		if !ok {
			http.NotFound(w, r)
			return
		}
		org, err := s.store.Role(id, u.ID)
		if err != nil {
			s.htmlErr(w, r, err)
			return
		}
		p, err := s.store.Poll(id)
		if err != nil {
			s.htmlErr(w, r, err)
			return
		}
		h(w, r, pollCtx{User: u, Poll: p, Organizer: org})
	})
}

// organizer is member for what only organizers may do.
func (s *Server) organizer(h pollHandler) http.HandlerFunc {
	return s.member(func(w http.ResponseWriter, r *http.Request, c pollCtx) {
		if !c.Organizer {
			http.Error(w, "only the poll's organizers can do that", http.StatusForbidden)
			return
		}
		h(w, r, c)
	})
}

// ---- welcome / invites / profile --------------------------------------

type simplePage struct {
	Title, Heading, Subtitle, Action, Next, Button, Error string
	NeedName, ShowToken                                   bool
	// Invite describes the poll a link is for, on the page it opens.
	Invite *invitePreview
	// OG is what a chat app shows when the link is pasted into it.
	OG *openGraph
}

type invitePreview struct {
	Title, Category, Status string
	People                  []store.Participant
}

type openGraph struct{ Title, Description, URL string }

func (s *Server) renderTmpl(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func welcomePage(next, errMsg string) simplePage {
	return simplePage{
		Title:     "Welcome",
		Heading:   "Welcome to Halfway",
		Subtitle:  "What should we call you? Your name is shown next to your answers, and it is all Halfway ever asks for.",
		Action:    "/welcome",
		Next:      safeNext(next),
		Button:    "Get started",
		Error:     errMsg,
		NeedName:  true,
		ShowToken: true,
	}
}

func (s *Server) welcomeGet(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Query().Get("next")
	if userFrom(r) != nil {
		http.Redirect(w, r, safeNext(next), http.StatusSeeOther)
		return
	}
	s.renderTmpl(w, http.StatusOK, "welcome.html", welcomePage(next, ""))
}

// tooManySignups turns the sign-up form down without losing the page.
func (s *Server) tooManySignups(w http.ResponseWriter, r *http.Request) {
	s.renderTmpl(w, http.StatusTooManyRequests, "welcome.html",
		welcomePage(r.FormValue("next"), "Too many new names from this connection. Try again in a minute."))
}

func (s *Server) welcomePost(w http.ResponseWriter, r *http.Request) {
	next := r.FormValue("next")
	_, token, err := s.store.CreateUser(r.FormValue("name"))
	if errors.Is(err, store.ErrInvalid) {
		s.renderTmpl(w, http.StatusBadRequest, "welcome.html", welcomePage(next, "Please enter a name (up to 40 characters)."))
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	setSession(w, r, token)
	http.Redirect(w, r, safeNext(next), http.StatusSeeOther)
}

// welcomeToken signs in with an existing token, e.g. on a second device.
func (s *Server) welcomeToken(w http.ResponseWriter, r *http.Request) {
	next := r.FormValue("next")
	token := strings.TrimSpace(r.FormValue("token"))
	if _, err := s.store.UserByToken(token); err != nil {
		s.renderTmpl(w, http.StatusBadRequest, "welcome.html", welcomePage(next, "That key wasn't recognised."))
		return
	}
	setSession(w, r, token)
	http.Redirect(w, r, safeNext(next), http.StatusSeeOther)
}

// statusLine is a poll's news in a few words, for link previews:
// "✅ Thu 8 Oct, 19:30", or "3 of 5 answered · decides Wed 7 Oct, 18:00".
func (s *Server) statusLine(p store.Poll) string {
	switch {
	case p.IsConfirmed():
		slots, _ := s.store.Slots(p.ID)
		for _, sl := range slots {
			if sl.ID == p.ChosenSlot {
				line := "✅ " + store.Label(sl.Start)
				if v := s.chosenVenue(p); v != nil {
					line += " · " + v.Name
				}
				return line
			}
		}
		return "✅ Decided"
	case p.IsCancelled():
		return "❌ Called off"
	}
	people, _ := s.store.Participants(p.ID)
	answered := 0
	for _, pp := range people {
		if pp.Answered {
			answered++
		}
	}
	line := strconv.Itoa(answered) + " of " + strconv.Itoa(len(people)) + " answered · decides " + store.Label(p.Deadline)
	if p.Quorum > 0 {
		line += " · on if " + strconv.Itoa(p.Quorum) + " can come"
	}
	return line
}

// joinPage is what an invite or organizer link shows someone who is not on
// the poll yet.
func (s *Server) joinPage(r *http.Request, p store.Poll, action string, organizer bool, errMsg string) simplePage {
	people, _ := s.store.Participants(p.ID)
	status := s.statusLine(p)
	pg := simplePage{
		Title:     "Join " + p.Title,
		Heading:   "Join “" + p.Title + "”",
		Subtitle:  "You've been invited to say when you can make it. Pick a name so the others can tell who's who.",
		Action:    action,
		Next:      action,
		Button:    "Join poll",
		Error:     errMsg,
		NeedName:  userFrom(r) == nil,
		ShowToken: userFrom(r) == nil,
		Invite:    &invitePreview{Title: p.Title, Category: p.Category, Status: status, People: people},
		OG:        &openGraph{Title: p.Title, Description: status, URL: inviteURL(r, p)},
	}
	if organizer {
		pg.Title = "Organize " + p.Title
		pg.Heading = "Organize “" + p.Title + "”"
		pg.Subtitle = "This link makes you an organizer: you can change the poll, decide it and invite others. Pick a name first."
		pg.Button = "Join as organizer"
		pg.OG = nil // an organizer link is not for sharing around
	}
	return pg
}

// closedPage is what a closed invite link shows newcomers. The preview still
// carries the poll's news, so the link can be posted again to share the result.
func (s *Server) closedPage(r *http.Request, p store.Poll) simplePage {
	status := s.statusLine(p)
	return simplePage{
		Title:    p.Title,
		Heading:  "This invite link is closed",
		Subtitle: "The organizer has stopped new people joining “" + p.Title + "”. If you should be on it, ask them for a new link.",
		Invite:   &invitePreview{Title: p.Title, Category: p.Category, Status: status},
		OG:       &openGraph{Title: p.Title, Description: status, URL: inviteURL(r, p)},
	}
}

func goneLink() simplePage {
	return simplePage{
		Title:    "Link not found",
		Heading:  "This link doesn't lead anywhere",
		Subtitle: "It may have been replaced by a new one, or the poll was deleted. Ask whoever sent it for the current link.",
	}
}

// inviteGet is where an invite link leads. Somebody with a name joins at
// once and lands on the poll, as the link is all the asking there is; anybody
// else is asked for a name first.
func (s *Server) inviteGet(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.PollByInvite(r.PathValue("code"))
	if err != nil {
		s.renderTmpl(w, http.StatusNotFound, "welcome.html", goneLink())
		return
	}
	u := userFrom(r)
	if u != nil {
		if _, err := s.store.Role(p.ID, u.ID); err == nil {
			http.Redirect(w, r, pollPath(p.ID), http.StatusSeeOther)
			return
		}
	}
	if !p.InviteOpen {
		s.renderTmpl(w, http.StatusOK, "welcome.html", s.closedPage(r, p))
		return
	}
	if u != nil {
		if err := s.store.Join(p.ID, u.ID, false); err != nil {
			s.fail(w, err)
			return
		}
		http.Redirect(w, r, pollPath(p.ID), http.StatusSeeOther)
		return
	}
	s.renderTmpl(w, http.StatusOK, "welcome.html", s.joinPage(r, p, "/i/"+p.InviteCode, false, ""))
}

func (s *Server) invitePost(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.PollByInvite(r.PathValue("code"))
	if err != nil {
		s.renderTmpl(w, http.StatusNotFound, "welcome.html", goneLink())
		return
	}
	if !p.InviteOpen {
		if u := userFrom(r); u == nil || !s.isMember(p.ID, u.ID) {
			s.renderTmpl(w, http.StatusForbidden, "welcome.html", s.closedPage(r, p))
			return
		}
	}
	s.join(w, r, p, "/i/"+p.InviteCode, false)
}

// organizerGet is where an organizer link leads: like an invite, but whoever
// follows it can run the poll too.
func (s *Server) organizerGet(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.PollByOrganizerCode(r.PathValue("code"))
	if err != nil {
		s.renderTmpl(w, http.StatusNotFound, "welcome.html", goneLink())
		return
	}
	if u := userFrom(r); u != nil {
		if err := s.store.Join(p.ID, u.ID, true); err != nil {
			s.fail(w, err)
			return
		}
		http.Redirect(w, r, pollPath(p.ID), http.StatusSeeOther)
		return
	}
	s.renderTmpl(w, http.StatusOK, "welcome.html", s.joinPage(r, p, "/o/"+p.OrganizerCode, true, ""))
}

func (s *Server) organizerPost(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.PollByOrganizerCode(r.PathValue("code"))
	if err != nil {
		s.renderTmpl(w, http.StatusNotFound, "welcome.html", goneLink())
		return
	}
	s.join(w, r, p, "/o/"+p.OrganizerCode, true)
}

// join puts the caller on p, making them a name first if they have none.
func (s *Server) join(w http.ResponseWriter, r *http.Request, p store.Poll, action string, organizer bool) {
	u := userFrom(r)
	if u == nil {
		// Joining without a name creates one, so it counts against the same
		// limit as signing up; otherwise one invite link would be a way
		// round it. Someone already signed in is not creating anybody.
		if !s.signups.allow(clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			s.renderTmpl(w, http.StatusTooManyRequests, "welcome.html",
				s.joinPage(r, p, action, organizer, "Too many new names from this connection. Try again in a minute."))
			return
		}
		nu, token, err := s.store.CreateUser(r.FormValue("name"))
		if errors.Is(err, store.ErrInvalid) {
			s.renderTmpl(w, http.StatusBadRequest, "welcome.html", s.joinPage(r, p, action, organizer, "Please enter a name (up to 40 characters)."))
			return
		} else if err != nil {
			s.fail(w, err)
			return
		}
		setSession(w, r, token)
		u = &nu
	}
	if err := s.store.Join(p.ID, u.ID, organizer); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, pollPath(p.ID), http.StatusSeeOther)
}

func (s *Server) isMember(pollID, uid int64) bool {
	_, err := s.store.Role(pollID, uid)
	return err == nil
}

// meGet is where old links to a profile page go: it is a dialog on every page.
func (s *Server) meGet(w http.ResponseWriter, r *http.Request, u *store.User) {
	http.Redirect(w, r, "/#profile", http.StatusSeeOther)
}

// meToken hands the caller their own sign-in key. It is fetched on demand
// when they choose to reveal or copy it, rather than being written into every
// page, and it is never cached.
func (s *Server) meToken(w http.ResponseWriter, r *http.Request, u *store.User) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"token": tokenFrom(r)})
}

// meTokenSaved records that someone has their key somewhere safe. There is
// no way back from losing it, so until this is set every page says so.
func (s *Server) meTokenSaved(w http.ResponseWriter, r *http.Request, u *store.User) {
	if err := s.store.MarkTokenSaved(u.ID); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, backTo(r, "/"), http.StatusSeeOther)
}

// mePost renames the caller. A blank or over-long name leaves it unchanged
// (the form's input already enforces both).
func (s *Server) mePost(w http.ResponseWriter, r *http.Request, u *store.User) {
	if err := s.store.RenameUser(u.ID, r.FormValue("name")); err != nil && !errors.Is(err, store.ErrInvalid) {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, backTo(r, "/"), http.StatusSeeOther)
}

// ---- pages ------------------------------------------------------------

type pageData struct {
	View string // "dashboard", "answer", "new" or "poll"
	User *store.User
	Path string // this page's URL, so actions can return here
	Now  time.Time
	// NowStamp is Now as a Stamp, to compare times with.
	NowStamp string

	// The sidebar and the lists: every poll the user is on.
	Polls         []store.Summary
	Sidebar       []store.Summary
	Groups        []group
	ToAnswer      []store.Summary
	UnseenCount   int
	ToAnswerCount int

	// The new poll form.
	Form  newForm
	Error string

	// One poll.
	Poll         *store.Poll
	IsOrganizer  bool
	Slots        []slotRow
	People       []store.Participant
	Answered     int
	Waiting      []store.Participant
	Leading      *slotRow
	Chosen       *slotRow
	InviteURL    string
	OrganizerURL string
	Chats        []store.Chat
	TelegramLink string
	// LastOrganizer is set when the user is the poll's only organizer, who
	// cannot leave it.
	LastOrganizer bool
	// Result is the decision as a message to send anywhere, and ResultBody
	// the same without the link, for share links that add the link themselves.
	Result, ResultBody string

	// PlacesOn is whether this server can find places; Places is the poll's
	// Where card, for a poll that finds one.
	PlacesOn bool
	Places   placesData
}

// group is a heading on the dashboard and the polls under it.
type group struct {
	Title, Icon, Color string
	Polls              []store.Summary
}

// slotRow is one of a poll's times, as its page shows it.
type slotRow struct {
	store.Slot
	Mine     int  // the user's answer
	Answered bool // whether they gave one
	Leading  bool
	Chosen   bool
	Yes      []store.Vote
	Maybe    []store.Vote
}

// slotView is what the "slotrow" template renders.
type slotView struct {
	slotRow
	Page pageData
}

// newForm is what the new poll form was filled in with, when it comes back
// to be corrected.
type newForm struct {
	Title, Category, Deadline, Quorum string
	Places                            bool
	Slots                             []string
}

// basePage fills in what every page with the sidebar needs.
func (s *Server) basePage(r *http.Request, u *store.User, view string) (pageData, error) {
	now := s.now()
	d := pageData{View: view, User: u, Path: r.URL.RequestURI(), Now: now, NowStamp: now.Format(store.Stamp), PlacesOn: s.finder != nil}
	polls, err := s.store.Polls(u.ID)
	if err != nil {
		return d, err
	}
	d.Polls = polls
	var waiting, open, coming, earlier []store.Summary
	for _, p := range polls {
		if p.Unseen {
			d.UnseenCount++
		}
		switch {
		case p.IsOpen() && !p.Mine:
			waiting = append(waiting, p)
			d.ToAnswer = append(d.ToAnswer, p)
		case p.IsOpen():
			open = append(open, p)
		case p.IsConfirmed() && !p.Past(d.NowStamp):
			coming = append(coming, p)
		default:
			earlier = append(earlier, p)
		}
	}
	d.ToAnswerCount = len(d.ToAnswer)
	d.Sidebar = append(append(append(d.Sidebar, waiting...), open...), coming...)
	// Most recent first: what just happened, or was just called off.
	for i, j := 0, len(earlier)-1; i < j; i, j = i+1, j-1 {
		earlier[i], earlier[j] = earlier[j], earlier[i]
	}
	for _, g := range []group{
		{"Waiting for your answer", "hand-finger", "orange", waiting},
		{"Open", "clock", "blue", open},
		{"Coming up", "calendar-check", "green", coming},
		{"Earlier", "history", "secondary", earlier},
	} {
		if len(g.Polls) > 0 {
			d.Groups = append(d.Groups, g)
		}
	}
	return d, nil
}

// backTo is where an action should send the browser: the page it came from
// (the form's "next" field), or fallback.
func backTo(r *http.Request, fallback string) string {
	if next := r.FormValue("next"); next != "" {
		return safeNext(next)
	}
	return fallback
}

func (s *Server) pageDashboard(w http.ResponseWriter, r *http.Request, u *store.User) {
	d, err := s.basePage(r, u, "dashboard")
	if err != nil {
		s.fail(w, err)
		return
	}
	s.renderTmpl(w, http.StatusOK, "page.html", d)
}

func (s *Server) pageAnswer(w http.ResponseWriter, r *http.Request, u *store.User) {
	d, err := s.basePage(r, u, "answer")
	if err != nil {
		s.fail(w, err)
		return
	}
	s.renderTmpl(w, http.StatusOK, "page.html", d)
}

func (s *Server) pageNew(w http.ResponseWriter, r *http.Request, u *store.User) {
	d, err := s.basePage(r, u, "new")
	if err != nil {
		s.fail(w, err)
		return
	}
	d.Form = newForm{Category: "dinner", Slots: []string{"", "", ""}, Places: s.finder != nil}
	s.renderTmpl(w, http.StatusOK, "page.html", d)
}

func (s *Server) formCreate(w http.ResponseWriter, r *http.Request, u *store.User) {
	r.ParseForm()
	form := newForm{
		Title: r.FormValue("title"), Category: r.FormValue("category"),
		Deadline: r.FormValue("deadline"), Quorum: strings.TrimSpace(r.FormValue("quorum")), Slots: r.Form["slot"],
		Places: r.FormValue("places") == "on",
	}
	quorum := 0
	bad := ""
	if form.Quorum != "" {
		var err error
		if quorum, err = strconv.Atoi(form.Quorum); err != nil || quorum < 2 || quorum > 100 {
			bad = "The minimum has to be a number of people from 2 to 100, or left empty."
		}
	}
	var p store.Poll
	if bad == "" {
		var err error
		p, err = s.store.CreatePoll(u.ID, store.NewPoll{
			Title: form.Title, Category: form.Category, Deadline: form.Deadline, Quorum: quorum,
			Slots: form.Slots, Origin: baseURL(r), Places: form.Places && s.finder != nil,
		}, s.now())
		if errors.Is(err, store.ErrInvalid) {
			bad = "Please give the poll a name, between one and " + strconv.Itoa(store.MaxSlots) +
				" times that are still to come, and a deadline no later than the first of them."
		} else if err != nil {
			s.fail(w, err)
			return
		}
	}
	if bad != "" {
		d, err := s.basePage(r, u, "new")
		if err != nil {
			s.fail(w, err)
			return
		}
		for len(form.Slots) < 3 {
			form.Slots = append(form.Slots, "")
		}
		d.Form, d.Error = form, bad
		s.renderTmpl(w, http.StatusBadRequest, "page.html", d)
		return
	}
	// Straight to the invite link: sending it is the next thing to do.
	http.Redirect(w, r, pollPath(p.ID)+"#share", http.StatusSeeOther)
}

func (s *Server) pagePoll(w http.ResponseWriter, r *http.Request, c pollCtx) {
	// A poll whose deadline has passed is decided before it is shown, whether
	// or not the clock has got round to it.
	if ev, err := s.store.DecideIfDue(c.Poll.ID, s.now()); err != nil {
		s.fail(w, err)
		return
	} else if ev != nil {
		s.kick()
		if c.Poll, err = s.store.Poll(c.Poll.ID); err != nil {
			s.fail(w, err)
			return
		}
	}
	// Looking at it is seeing its news.
	if _, err := s.store.MarkSeen(c.Poll.ID, c.User.ID); err != nil {
		s.fail(w, err)
		return
	}
	d, err := s.basePage(r, c.User, "poll")
	if err != nil {
		s.fail(w, err)
		return
	}
	p := c.Poll
	d.Poll, d.IsOrganizer = &p, c.Organizer
	d.InviteURL = inviteURL(r, p)
	if c.Organizer {
		d.OrganizerURL = organizerURL(r, p)
		if d.Chats, err = s.store.Chats(p.ID); err != nil {
			s.fail(w, err)
			return
		}
		if s.bot != nil {
			d.TelegramLink = s.bot.AddLink(p.ChatCode)
		}
	}
	if d.People, err = s.store.Participants(p.ID); err != nil {
		s.fail(w, err)
		return
	}
	organizers := 0
	for _, pp := range d.People {
		if pp.Answered {
			d.Answered++
		} else {
			d.Waiting = append(d.Waiting, pp)
		}
		if pp.Organizer {
			organizers++
		}
	}
	d.LastOrganizer = c.Organizer && organizers == 1
	slots, err := s.store.Slots(p.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if p.Places {
		if d.Places, err = s.placesFor(p, c.User.ID); err != nil {
			s.fail(w, err)
			return
		}
	}
	lead, hasLead := store.Leading(slots)
	for _, sl := range slots {
		row := slotRow{Slot: sl}
		row.Mine, row.Answered = sl.Answer(c.User.ID)
		row.Leading = p.IsOpen() && hasLead && sl.ID == lead.ID
		row.Chosen = p.IsConfirmed() && sl.ID == p.ChosenSlot
		for _, v := range sl.Votes {
			switch v.Answer {
			case store.Yes:
				row.Yes = append(row.Yes, v)
			case store.IfNeeded:
				row.Maybe = append(row.Maybe, v)
			}
		}
		d.Slots = append(d.Slots, row)
	}
	for i := range d.Slots {
		if d.Slots[i].Leading {
			d.Leading = &d.Slots[i]
		}
		if d.Slots[i].Chosen {
			d.Chosen = &d.Slots[i]
		}
	}
	d.Result, d.ResultBody = resultMessage(p, d.Chosen, d.Places.Chosen, d.InviteURL)
	w.Header().Set("Cache-Control", "no-store")
	s.renderTmpl(w, http.StatusOK, "page.html", d)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request, u *store.User) {
	s.stream(w, r, u.ID)
}

// calendar is the decided time as a calendar file, for "Add to calendar".
// It is a download, not a subscription: nothing is told about changes.
func (s *Server) calendar(w http.ResponseWriter, r *http.Request, c pollCtx) {
	if !c.Poll.IsConfirmed() {
		http.NotFound(w, r)
		return
	}
	slots, err := s.store.Slots(c.Poll.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, sl := range slots {
		if sl.ID != c.Poll.ChosenSlot {
			continue
		}
		w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+fileName(c.Poll.Title)+`.ics"`)
		io.WriteString(w, icsEvent(c.Poll, sl, s.chosenVenue(c.Poll), inviteURL(r, c.Poll), s.now()))
		return
	}
	http.NotFound(w, r)
}

// resultMessage is a decision as a message to paste anywhere: a group chat
// with no bot in it, an email, a text. It comes back whole, and without its
// last line, the link to the poll.
func resultMessage(p store.Poll, chosen *slotRow, venue *venueRow, invite string) (string, string) {
	var lines []string
	switch {
	case p.IsConfirmed() && chosen != nil:
		lines = append(lines, "✅ "+p.Title+" is on: "+store.Label(chosen.Start))
		if venue != nil {
			where := "📍 " + venue.Name
			if venue.Address != "" {
				where += ", " + venue.Address
			}
			lines = append(lines, where, "Map: "+venue.MapURL)
		}
	case p.IsCancelled():
		reason := "nobody could make any of the times"
		if p.Quorum > 0 {
			reason = "not enough people could come"
		}
		lines = append(lines, "❌ "+p.Title+" is called off: "+reason+".")
	default:
		return "", ""
	}
	body := strings.Join(lines, "\n")
	return body + "\nPoll: " + invite, body
}

// chosenVenue is the place a poll was decided for, if it has one.
func (s *Server) chosenVenue(p store.Poll) *store.Venue {
	if p.ChosenVenue == 0 {
		return nil
	}
	venues, err := s.store.Venues(p.ID)
	if err != nil {
		return nil
	}
	for _, v := range venues {
		if v.ID == p.ChosenVenue {
			return &v
		}
	}
	return nil
}

// fileName makes a title safe to offer as the name of a download.
func fileName(title string) string {
	var b strings.Builder
	for _, r := range title {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "halfway"
	}
	return b.String()
}

// icsText escapes text for a calendar file (RFC 5545).
func icsText(s string) string {
	return strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\n", `\n`, "\r", "").Replace(s)
}

// uidHost is the host a poll was made at, which makes its calendar event's
// id unique to this server.
func uidHost(origin string) string {
	if u, err := url.Parse(origin); err == nil && u.Host != "" {
		return u.Host
	}
	return "halfway"
}

// icsEvent writes one event. Its times are "floating", local wherever the
// calendar is, which is what "Thursday at 19:30" means to the people going.
func icsEvent(p store.Poll, sl store.Slot, venue *store.Venue, link string, now time.Time) string {
	start, _ := stampTime(sl.Start)
	length := eventLength[p.Category]
	if length == 0 {
		length = 2 * time.Hour
	}
	const floating = "20060102T150405"
	lines := []string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//Halfway//EN",
		"CALSCALE:GREGORIAN",
		"BEGIN:VEVENT",
		"UID:halfway-poll-" + strconv.FormatInt(p.ID, 10) + "@" + uidHost(p.Origin),
		"DTSTAMP:" + now.UTC().Format("20060102T150405Z"),
		"DTSTART:" + start.Format(floating),
		"DTEND:" + start.Add(length).Format(floating),
		"SUMMARY:" + icsText(p.Title),
		"DESCRIPTION:" + icsText("Decided with Halfway: "+link),
	}
	if venue != nil {
		where := venue.Name
		if venue.Address != "" {
			where += ", " + venue.Address
		}
		lines = append(lines, "LOCATION:"+icsText(where),
			"GEO:"+strconv.FormatFloat(venue.At.Lat, 'f', 5, 64)+";"+strconv.FormatFloat(venue.At.Lon, 'f', 5, 64))
	}
	lines = append(lines,
		"URL:"+link,
		"END:VEVENT",
		"END:VCALENDAR",
	)
	return strings.Join(lines, "\r\n") + "\r\n"
}

// ---- forms on a poll ----------------------------------------------------

func (s *Server) formVote(w http.ResponseWriter, r *http.Request, c pollCtx) {
	slot, ok := pathID(r, "slot")
	answer, err := strconv.Atoi(r.FormValue("answer"))
	if !ok || err != nil {
		http.Error(w, "bad answer", http.StatusBadRequest)
		return
	}
	ev, err := s.store.Vote(c.Poll.ID, slot, c.User.ID, answer, s.now())
	if ev != nil {
		s.kick()
	}
	switch {
	case err == nil, errors.Is(err, store.ErrClosed):
		// A poll decided while this was on its way just shows the decision.
		http.Redirect(w, r, backTo(r, pollPath(c.Poll.ID)), http.StatusSeeOther)
	case errors.Is(err, store.ErrInvalid):
		http.Error(w, "bad answer", http.StatusBadRequest)
	default:
		s.htmlErr(w, r, err)
	}
}

func (s *Server) formLeave(w http.ResponseWriter, r *http.Request, c pollCtx) {
	err := s.store.Leave(c.Poll.ID, c.User.ID)
	if errors.Is(err, store.ErrInvalid) {
		http.Error(w, "you are this poll's only organizer: make someone else one first, or delete it", http.StatusConflict)
		return
	} else if err != nil {
		s.htmlErr(w, r, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) formRename(w http.ResponseWriter, r *http.Request, c pollCtx) {
	if err := s.store.Rename(c.Poll.ID, r.FormValue("title")); err != nil && !errors.Is(err, store.ErrInvalid) {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, backTo(r, pollPath(c.Poll.ID)), http.StatusSeeOther)
}

func (s *Server) formSettings(w http.ResponseWriter, r *http.Request, c pollCtx) {
	quorum := 0
	if q := strings.TrimSpace(r.FormValue("quorum")); q != "" {
		var err error
		if quorum, err = strconv.Atoi(q); err != nil {
			http.Error(w, "the minimum is a number of people", http.StatusBadRequest)
			return
		}
	}
	ev, err := s.store.Reschedule(c.Poll.ID, r.FormValue("deadline"), quorum, s.now())
	if ev != nil {
		s.kick()
	}
	switch {
	case err == nil, errors.Is(err, store.ErrClosed):
		http.Redirect(w, r, backTo(r, pollPath(c.Poll.ID)), http.StatusSeeOther)
	case errors.Is(err, store.ErrInvalid):
		http.Error(w, "the deadline has to be still to come and no later than the first time, and the minimum from 2 to 100", http.StatusBadRequest)
	default:
		s.htmlErr(w, r, err)
	}
}

func (s *Server) formDecide(w http.ResponseWriter, r *http.Request, c pollCtx) {
	ev, err := s.store.DecideNow(c.Poll.ID, s.now())
	if ev != nil {
		s.kick()
	}
	if err != nil && !errors.Is(err, store.ErrClosed) {
		s.htmlErr(w, r, err)
		return
	}
	http.Redirect(w, r, backTo(r, pollPath(c.Poll.ID)), http.StatusSeeOther)
}

func (s *Server) formPick(w http.ResponseWriter, r *http.Request, c pollCtx) {
	slot, ok := pathID(r, "slot")
	if !ok {
		http.NotFound(w, r)
		return
	}
	ev, err := s.store.Pick(c.Poll.ID, slot)
	if err != nil {
		s.htmlErr(w, r, err)
		return
	}
	if ev != nil {
		s.kick()
	}
	http.Redirect(w, r, backTo(r, pollPath(c.Poll.ID)), http.StatusSeeOther)
}

func (s *Server) formInviteReset(w http.ResponseWriter, r *http.Request, c pollCtx) {
	if err := s.store.ResetInvite(c.Poll.ID); err != nil {
		s.htmlErr(w, r, err)
		return
	}
	http.Redirect(w, r, pollPath(c.Poll.ID)+"#share", http.StatusSeeOther)
}

func (s *Server) formInviteOpen(open bool) pollHandler {
	return func(w http.ResponseWriter, r *http.Request, c pollCtx) {
		if err := s.store.SetInviteOpen(c.Poll.ID, open); err != nil {
			s.htmlErr(w, r, err)
			return
		}
		http.Redirect(w, r, backTo(r, pollPath(c.Poll.ID)), http.StatusSeeOther)
	}
}

func (s *Server) formOrganizerReset(w http.ResponseWriter, r *http.Request, c pollCtx) {
	if err := s.store.ResetOrganizerLink(c.Poll.ID); err != nil {
		s.htmlErr(w, r, err)
		return
	}
	http.Redirect(w, r, pollPath(c.Poll.ID)+"#share", http.StatusSeeOther)
}

// formRemovePerson takes somebody off the poll, such as the second entry of
// someone who joined again from a new device.
func (s *Server) formRemovePerson(w http.ResponseWriter, r *http.Request, c pollCtx) {
	uid, ok := pathID(r, "user")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.store.Leave(c.Poll.ID, uid); err != nil && !errors.Is(err, store.ErrInvalid) {
		s.htmlErr(w, r, err)
		return
	}
	http.Redirect(w, r, backTo(r, pollPath(c.Poll.ID)), http.StatusSeeOther)
}

func (s *Server) formRemoveChat(w http.ResponseWriter, r *http.Request, c pollCtx) {
	id, ok := pathID(r, "chat")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.store.RemoveChat(c.Poll.ID, id); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, backTo(r, pollPath(c.Poll.ID)), http.StatusSeeOther)
}

// formDelete deletes a poll for everyone on it. The dashboard it lands on
// offers to undo that.
func (s *Server) formDelete(w http.ResponseWriter, r *http.Request, c pollCtx) {
	if err := s.store.DeletePoll(c.Poll.ID); err != nil {
		s.htmlErr(w, r, err)
		return
	}
	http.Redirect(w, r, "/?undo="+strconv.FormatInt(c.Poll.ID, 10), http.StatusSeeOther)
}

func (s *Server) formRestore(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.store.RestorePoll(id, u.ID); err != nil {
		s.htmlErr(w, r, err)
		return
	}
	http.Redirect(w, r, pollPath(id), http.StatusSeeOther)
}

// ---- helpers ----------------------------------------------------------

func pathID(r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	return id, err == nil && id > 0
}

// htmlErr maps a store error to a plain HTTP error page.
func (s *Server) htmlErr(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	s.fail(w, err)
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	log.Printf("web: %v", err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}
