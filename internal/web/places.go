package web

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"halfway/internal/places"
	"halfway/internal/store"
)

// Finding a place to meet. All of it is on the server's side: the browser
// asks this server, and this server asks OpenStreetMap, so no third party
// learns who is looking for what.

// suggestions is how many places Halfway suggests at a time.
const suggestions = 3

// venueRow is one place on a poll's page.
type venueRow struct {
	store.Venue
	Fair    places.Fairness
	Fairest bool // the shortest longest trip of them all
	Leading bool // what an open poll is heading for
	Chosen  bool
	Mine    bool // the viewer would go
	MapURL  string
}

// placesData is the Where card: the viewer's starting point and the places.
type placesData struct {
	Mine   *store.Start
	Starts int // how many people have given one
	Venues []venueRow
	Chosen *venueRow
}

// placesFor works out the Where card for user on poll p.
func (s *Server) placesFor(p store.Poll, user int64) (placesData, error) {
	var d placesData
	starts, err := s.store.Starts(p.ID)
	if err != nil {
		return d, err
	}
	venues, err := s.store.Venues(p.ID)
	if err != nil {
		return d, err
	}
	d.Starts = len(starts)
	for i := range starts {
		if starts[i].UserID == user {
			d.Mine = &starts[i]
		}
	}
	ps := store.PlacesStarts(starts)
	lead, hasLead := store.BestVenue(venues, starts)
	fairest := -1
	for i, v := range venues {
		row := venueRow{Venue: v, Fair: places.Measure(v.At, ps), MapURL: places.MapURL(v.At)}
		row.Chosen = p.IsConfirmed() && v.ID == p.ChosenVenue
		row.Leading = p.IsOpen() && hasLead && v.ID == lead.ID && len(venues) > 1
		for _, voter := range v.Voters {
			row.Mine = row.Mine || voter.ID == user
		}
		if len(ps) > 0 && (fairest < 0 || places.Fairer(row.Fair, d.Venues[fairest].Fair)) {
			fairest = i
		}
		d.Venues = append(d.Venues, row)
	}
	if fairest >= 0 && len(d.Venues) > 1 {
		d.Venues[fairest].Fairest = true
	}
	for i := range d.Venues {
		if d.Venues[i].Chosen {
			d.Chosen = &d.Venues[i]
		}
	}
	return d, nil
}

// placesPoll is member for what only makes sense on a poll that finds a
// place, on a server that can.
func (s *Server) placesPoll(h pollHandler) pollHandler {
	return func(w http.ResponseWriter, r *http.Request, c pollCtx) {
		if s.finder == nil || !c.Poll.Places {
			http.NotFound(w, r)
			return
		}
		h(w, r, c)
	}
}

// search looks up a neighbourhood, street or station for the viewer, near
// where the others are coming from.
func (s *Server) search(w http.ResponseWriter, r *http.Request, c pollCtx) {
	if !s.searches.allow("user:" + strconv.FormatInt(c.User.ID, 10)) {
		writeJSONError(w, http.StatusTooManyRequests, "Too many searches. Wait a few seconds.")
		return
	}
	starts, err := s.store.Starts(c.Poll.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	var near []places.Point
	for _, st := range starts {
		near = append(near, st.At)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	found, err := s.finder.Search(ctx, r.URL.Query().Get("q"), near)
	if err != nil {
		log.Printf("places: search: %v", err)
		writeJSONError(w, http.StatusBadGateway, "Couldn't reach the map service. Try again in a moment.")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(found)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// pointFrom reads a point posted by a form.
func pointFrom(r *http.Request) (places.Point, bool) {
	lat, err1 := strconv.ParseFloat(r.FormValue("lat"), 64)
	lon, err2 := strconv.ParseFloat(r.FormValue("lon"), 64)
	p := places.Point{Lat: lat, Lon: lon}
	return p, err1 == nil && err2 == nil && p.Valid()
}

func (s *Server) formStart(w http.ResponseWriter, r *http.Request, c pollCtx) {
	at, ok := pointFrom(r)
	if !ok {
		http.Error(w, "that isn't a place on the map", http.StatusBadRequest)
		return
	}
	if err := s.store.SetStart(c.Poll.ID, c.User.ID, at, r.FormValue("label"), r.FormValue("mode")); err != nil {
		s.htmlErr(w, r, err)
		return
	}
	http.Redirect(w, r, backTo(r, pollPath(c.Poll.ID)), http.StatusSeeOther)
}

func (s *Server) formMode(w http.ResponseWriter, r *http.Request, c pollCtx) {
	if err := s.store.SetMode(c.Poll.ID, c.User.ID, r.FormValue("mode")); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, backTo(r, pollPath(c.Poll.ID)), http.StatusSeeOther)
}

func (s *Server) formClearStart(w http.ResponseWriter, r *http.Request, c pollCtx) {
	if err := s.store.ClearStart(c.Poll.ID, c.User.ID); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, backTo(r, pollPath(c.Poll.ID)), http.StatusSeeOther)
}

// formSuggest looks for places around where everyone is coming from and
// suggests the fairest. Anyone on the poll may ask, a few times a minute
// per poll, as each time is a request to OpenStreetMap.
func (s *Server) formSuggest(w http.ResponseWriter, r *http.Request, c pollCtx) {
	if !c.Poll.IsOpen() && !c.Organizer {
		http.Error(w, "this poll has been decided", http.StatusConflict)
		return
	}
	starts, err := s.store.Starts(c.Poll.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if len(starts) == 0 {
		http.Error(w, "First, say where you're coming from.", http.StatusConflict)
		return
	}
	if !s.suggests.allow("poll:" + strconv.FormatInt(c.Poll.ID, 10)) {
		http.Error(w, "Places were just looked for. Try again in a minute.", http.StatusTooManyRequests)
		return
	}
	var points []places.Point
	for _, st := range starts {
		points = append(points, st.At)
	}
	center, km := places.Area(points)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	found, err := s.finder.Nearby(ctx, c.Poll.Category, center, km)
	if err == nil && len(found) < suggestions {
		// Not much around: look twice as far before giving up.
		found, err = s.finder.Nearby(ctx, c.Poll.Category, center, km*2)
	}
	if err != nil {
		log.Printf("places: nearby: %v", err)
		http.Error(w, "Couldn't reach the map service. Try again in a moment.", http.StatusBadGateway)
		return
	}
	picked := places.Suggest(found, store.PlacesStarts(starts), suggestions)
	if len(picked) == 0 {
		http.Error(w, "Nothing suitable was found nearby. An organizer can add a place by hand.", http.StatusUnprocessableEntity)
		return
	}
	if err := s.store.Suggest(c.Poll.ID, picked); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, backTo(r, pollPath(c.Poll.ID)), http.StatusSeeOther)
}

// formAddVenue is an organizer adding a place of their own choosing, found
// with the same search as a starting point. The search's label is split
// into the name and the rest, which is near enough an address.
func (s *Server) formAddVenue(w http.ResponseWriter, r *http.Request, c pollCtx) {
	at, ok := pointFrom(r)
	if !ok {
		http.Error(w, "that isn't a place on the map", http.StatusBadRequest)
		return
	}
	name, address, _ := strings.Cut(r.FormValue("label"), ",")
	if _, err := s.store.AddVenue(c.Poll.ID, c.User.ID, name, strings.TrimSpace(address), at); errors.Is(err, store.ErrInvalid) {
		http.Error(w, "A poll has room for "+strconv.Itoa(store.MaxVenues)+" places; remove one first.", http.StatusConflict)
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, backTo(r, pollPath(c.Poll.ID)), http.StatusSeeOther)
}

func (s *Server) formVenueVote(w http.ResponseWriter, r *http.Request, c pollCtx) {
	id, ok := pathID(r, "venue")
	if !ok {
		http.NotFound(w, r)
		return
	}
	err := s.store.ToggleVenueVote(c.Poll.ID, id, c.User.ID)
	if err != nil && !errors.Is(err, store.ErrClosed) {
		s.htmlErr(w, r, err)
		return
	}
	http.Redirect(w, r, backTo(r, pollPath(c.Poll.ID)), http.StatusSeeOther)
}

func (s *Server) formVenuePick(w http.ResponseWriter, r *http.Request, c pollCtx) {
	id, ok := pathID(r, "venue")
	if !ok {
		http.NotFound(w, r)
		return
	}
	ev, err := s.store.PickVenue(c.Poll.ID, id)
	if err != nil {
		s.htmlErr(w, r, err)
		return
	}
	if ev != nil {
		s.kick()
	}
	http.Redirect(w, r, backTo(r, pollPath(c.Poll.ID)), http.StatusSeeOther)
}

func (s *Server) formVenueRemove(w http.ResponseWriter, r *http.Request, c pollCtx) {
	id, ok := pathID(r, "venue")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.store.RemoveVenue(c.Poll.ID, id); errors.Is(err, store.ErrInvalid) {
		http.Error(w, "That is the place it was decided for. Pick another one first.", http.StatusConflict)
		return
	} else if err != nil {
		s.htmlErr(w, r, err)
		return
	}
	http.Redirect(w, r, backTo(r, pollPath(c.Poll.ID)), http.StatusSeeOther)
}
