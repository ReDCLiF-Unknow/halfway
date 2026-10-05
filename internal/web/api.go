package web

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"halfway/internal/store"
)

// The JSON API, which the command-line tool uses. Callers authenticate with
// "Authorization: Bearer <key>", the same sign-in key the profile shows. A
// poll the caller is not on looks exactly like one that does not exist.

func (s *Server) apiRoutes() {
	s.mux.HandleFunc("POST /api/users", s.limited(s.apiCreateUser, func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusTooManyRequests, "too many new people from this address; wait a minute")
	}))
	s.mux.HandleFunc("GET /api/me", s.api(s.apiMe))
	s.mux.HandleFunc("GET /api/polls", s.api(s.apiPolls))
	s.mux.HandleFunc("POST /api/polls", s.api(s.apiCreatePoll))
	s.mux.HandleFunc("POST /api/join", s.api(s.apiJoin))
	s.mux.HandleFunc("GET /api/polls/{id}", s.apiMember(s.apiPoll))
	s.mux.HandleFunc("POST /api/polls/{id}/answers", s.apiMember(s.apiAnswer))
	s.mux.HandleFunc("POST /api/polls/{id}/leave", s.apiMember(s.apiLeave))
	s.mux.HandleFunc("POST /api/polls/{id}/decide", s.apiOrganizer(s.apiDecide))
	s.mux.HandleFunc("POST /api/polls/{id}/pick", s.apiOrganizer(s.apiPick))
	s.mux.HandleFunc("DELETE /api/polls/{id}", s.apiOrganizer(s.apiDelete))
	s.mux.HandleFunc("POST /api/polls/{id}/restore", s.api(s.apiRestore))
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// apiErr answers a store error as JSON.
func (s *Server) apiErr(w http.ResponseWriter, err error, invalid string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrInvalid):
		writeError(w, http.StatusBadRequest, invalid)
	case errors.Is(err, store.ErrClosed):
		writeError(w, http.StatusConflict, "the poll has been decided")
	default:
		log.Printf("api: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// api is authed for the JSON API: no redirect, just a 401 saying what to do.
func (s *Server) api(h userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := userFrom(r)
		if u == nil {
			writeError(w, http.StatusUnauthorized, `missing or invalid key: run "halfway register <name>", or pass your sign-in key with -t`)
			return
		}
		h(w, r, u)
	}
}

func (s *Server) apiMember(h pollHandler) http.HandlerFunc {
	return s.api(func(w http.ResponseWriter, r *http.Request, u *store.User) {
		id, ok := pathID(r, "id")
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid poll id")
			return
		}
		org, err := s.store.Role(id, u.ID)
		if err != nil {
			s.apiErr(w, err, "")
			return
		}
		p, err := s.store.Poll(id)
		if err != nil {
			s.apiErr(w, err, "")
			return
		}
		h(w, r, pollCtx{User: u, Poll: p, Organizer: org})
	})
}

func (s *Server) apiOrganizer(h pollHandler) http.HandlerFunc {
	return s.apiMember(func(w http.ResponseWriter, r *http.Request, c pollCtx) {
		if !c.Organizer {
			writeError(w, http.StatusForbidden, "only the poll's organizers can do that")
			return
		}
		h(w, r, c)
	})
}

func (s *Server) apiCreateUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	u, token, err := s.store.CreateUser(in.Name)
	if err != nil {
		s.apiErr(w, err, "a name is 1 to 40 characters")
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		store.User
		Token string `json:"token"`
	}{u, token})
}

func (s *Server) apiMe(w http.ResponseWriter, r *http.Request, u *store.User) {
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) apiPolls(w http.ResponseWriter, r *http.Request, u *store.User) {
	polls, err := s.store.Polls(u.ID)
	if err != nil {
		s.apiErr(w, err, "")
		return
	}
	writeJSON(w, http.StatusOK, polls)
}

func (s *Server) apiCreatePoll(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		Title    string   `json:"title"`
		Category string   `json:"category"`
		Times    []string `json:"times"`
		Deadline string   `json:"deadline"`
		Quorum   int      `json:"quorum"`
		Places   bool     `json:"places"`
	}
	if !decode(w, r, &in) {
		return
	}
	// "2026-10-09 19:30" is as good as the form's "2026-10-09T19:30".
	t := func(s string) string { return strings.Replace(strings.TrimSpace(s), " ", "T", 1) }
	for i := range in.Times {
		in.Times[i] = t(in.Times[i])
	}
	p, err := s.store.CreatePoll(u.ID, store.NewPoll{
		Title: in.Title, Category: in.Category, Slots: in.Times, Deadline: t(in.Deadline), Quorum: in.Quorum,
		Places: in.Places && s.finder != nil, Origin: baseURL(r),
	}, s.now())
	if err != nil {
		s.apiErr(w, err, "a poll needs a title of up to 80 characters, 1 to 8 times still to come (YYYY-MM-DD HH:MM), "+
			"a deadline no later than the first, and a minimum of 2 to 100 people or none")
		return
	}
	s.apiPollView(w, r, http.StatusCreated, pollCtx{User: u, Poll: p, Organizer: true})
}

// apiJoin joins a poll by its invite or organizer link, or just the code
// at the end of one.
func (s *Server) apiJoin(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		Link string `json:"link"`
	}
	if !decode(w, r, &in) {
		return
	}
	link := strings.TrimRight(strings.TrimSpace(in.Link), "/")
	organizer := strings.Contains(link, "/o/")
	code := link[strings.LastIndex(link, "/")+1:]
	var p store.Poll
	var err error
	if organizer {
		p, err = s.store.PollByOrganizerCode(code)
	} else if p, err = s.store.PollByInvite(code); errors.Is(err, store.ErrNotFound) {
		p, err = s.store.PollByOrganizerCode(code)
		organizer = err == nil
	}
	if err != nil {
		writeError(w, http.StatusNotFound, "that link isn't for any poll")
		return
	}
	if !organizer && !p.InviteOpen && !s.isMember(p.ID, u.ID) {
		writeError(w, http.StatusForbidden, "that invite link is closed to newcomers")
		return
	}
	if err := s.store.Join(p.ID, u.ID, organizer); err != nil {
		s.apiErr(w, err, "")
		return
	}
	org, _ := s.store.Role(p.ID, u.ID)
	s.apiPollView(w, r, http.StatusOK, pollCtx{User: u, Poll: p, Organizer: org})
}

// apiTime is one of a poll's times, as the API gives it.
type apiTime struct {
	store.Slot
	Number  int    `json:"number"` // 1 for the earliest, as the CLI refers to it
	Mine    string `json:"your_answer,omitempty"`
	Leading bool   `json:"leading,omitempty"`
	Chosen  bool   `json:"chosen,omitempty"`
	Label   string `json:"label"`
	CanCome int    `json:"can_come"`
}

var answerNames = map[int]string{store.Yes: "yes", store.IfNeeded: "if-needed", store.No: "no"}

func (s *Server) apiPoll(w http.ResponseWriter, r *http.Request, c pollCtx) {
	if _, err := s.store.DecideIfDue(c.Poll.ID, s.now()); err != nil {
		s.apiErr(w, err, "")
		return
	}
	p, err := s.store.Poll(c.Poll.ID)
	if err != nil {
		s.apiErr(w, err, "")
		return
	}
	c.Poll = p
	s.apiPollView(w, r, http.StatusOK, c)
}

// apiPollView writes one poll as the caller sees it.
func (s *Server) apiPollView(w http.ResponseWriter, r *http.Request, status int, c pollCtx) {
	slots, err := s.store.Slots(c.Poll.ID)
	if err != nil {
		s.apiErr(w, err, "")
		return
	}
	people, err := s.store.Participants(c.Poll.ID)
	if err != nil {
		s.apiErr(w, err, "")
		return
	}
	lead, hasLead := store.Leading(slots)
	times := []apiTime{}
	for i, sl := range slots {
		t := apiTime{Slot: sl, Number: i + 1, Label: store.Label(sl.Start), CanCome: sl.CanCome()}
		if a, ok := sl.Answer(c.User.ID); ok {
			t.Mine = answerNames[a]
		}
		t.Leading = c.Poll.IsOpen() && hasLead && sl.ID == lead.ID
		t.Chosen = c.Poll.IsConfirmed() && sl.ID == c.Poll.ChosenSlot
		times = append(times, t)
	}
	out := map[string]any{
		"poll": c.Poll, "times": times, "people": people, "organizer": c.Organizer,
		"invite_url": inviteURL(r, c.Poll), "deadline_label": store.Label(c.Poll.Deadline),
	}
	if c.Organizer {
		out["organizer_url"] = organizerURL(r, c.Poll)
	}
	if v := s.chosenVenue(c.Poll); v != nil {
		out["venue"] = map[string]any{"name": v.Name, "address": v.Address, "lat": v.At.Lat, "lon": v.At.Lon}
	}
	writeJSON(w, status, out)
}

// apiAnswer records answers: {"time": 2, "answer": "yes"}, the time by its
// number, earliest first, or by "time_id".
func (s *Server) apiAnswer(w http.ResponseWriter, r *http.Request, c pollCtx) {
	var in struct {
		Time   int    `json:"time"`
		TimeID int64  `json:"time_id"`
		Answer string `json:"answer"`
	}
	if !decode(w, r, &in) {
		return
	}
	answer, ok := map[string]int{"yes": store.Yes, "if-needed": store.IfNeeded, "maybe": store.IfNeeded, "no": store.No}[strings.ToLower(in.Answer)]
	if !ok {
		writeError(w, http.StatusBadRequest, `answer with "yes", "if-needed" (or "maybe") or "no"`)
		return
	}
	id, ok := s.timeID(c.Poll.ID, in.Time, in.TimeID)
	if !ok {
		writeError(w, http.StatusBadRequest, "no such time on this poll")
		return
	}
	ev, err := s.store.Vote(c.Poll.ID, id, c.User.ID, answer, s.now())
	if ev != nil {
		s.kick()
	}
	if err != nil {
		s.apiErr(w, err, "invalid answer")
		return
	}
	if c.Poll, err = s.store.Poll(c.Poll.ID); err != nil {
		s.apiErr(w, err, "")
		return
	}
	s.apiPollView(w, r, http.StatusOK, c)
}

// timeID is the id of a poll's time given by its number (1 = earliest) or
// its id.
func (s *Server) timeID(pollID int64, number int, id int64) (int64, bool) {
	slots, err := s.store.Slots(pollID)
	if err != nil {
		return 0, false
	}
	for i, sl := range slots {
		if (number > 0 && i+1 == number) || (id > 0 && sl.ID == id) {
			return sl.ID, true
		}
	}
	return 0, false
}

func (s *Server) apiLeave(w http.ResponseWriter, r *http.Request, c pollCtx) {
	if err := s.store.Leave(c.Poll.ID, c.User.ID); err != nil {
		s.apiErr(w, err, "you are this poll's only organizer: make someone else one first, or delete it")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiDecide(w http.ResponseWriter, r *http.Request, c pollCtx) {
	ev, err := s.store.DecideNow(c.Poll.ID, s.now())
	if ev != nil {
		s.kick()
	}
	if err != nil {
		s.apiErr(w, err, "")
		return
	}
	s.apiPoll(w, r, c)
}

func (s *Server) apiPick(w http.ResponseWriter, r *http.Request, c pollCtx) {
	var in struct {
		Time   int   `json:"time"`
		TimeID int64 `json:"time_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	id, ok := s.timeID(c.Poll.ID, in.Time, in.TimeID)
	if !ok {
		writeError(w, http.StatusBadRequest, "no such time on this poll")
		return
	}
	ev, err := s.store.Pick(c.Poll.ID, id)
	if ev != nil {
		s.kick()
	}
	if err != nil {
		s.apiErr(w, err, "")
		return
	}
	s.apiPoll(w, r, c)
}

func (s *Server) apiDelete(w http.ResponseWriter, r *http.Request, c pollCtx) {
	if err := s.store.DeletePoll(c.Poll.ID); err != nil {
		s.apiErr(w, err, "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiRestore(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid poll id")
		return
	}
	if err := s.store.RestorePoll(id, u.ID); err != nil {
		s.apiErr(w, err, "")
		return
	}
	p, err := s.store.Poll(id)
	if err != nil {
		s.apiErr(w, err, "")
		return
	}
	s.apiPollView(w, r, http.StatusOK, pollCtx{User: u, Poll: p, Organizer: true})
}
