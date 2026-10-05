package places

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// Around Munich: Schwabing in the north, Giesing in the south-east,
// Pasing in the west.
var (
	schwabing = Point{48.1636, 11.5868}
	giesing   = Point{48.1110, 11.5960}
	pasing    = Point{48.1494, 11.4614}
	marienpl  = Point{48.1374, 11.5755}
)

func TestSnapKeepsOnlyAroundHere(t *testing.T) {
	p := Snap(Point{48.16371, 11.58689})
	if p != (Point{48.165, 11.585}) {
		t.Errorf("snapped to %+v", p)
	}
	if d := Km(Point{48.16371, 11.58689}, p); d > 0.4 {
		t.Errorf("snapping moved it %.2f km", d)
	}
	if Snap(p) != p {
		t.Error("snapping twice moved it again")
	}
}

func TestDistanceAndMinutes(t *testing.T) {
	d := Km(schwabing, giesing)
	if d < 5.5 || d > 6.2 {
		t.Errorf("Schwabing to Giesing is %.2f km, want about 5.9", d)
	}
	if Km(pasing, pasing) != 0 {
		t.Error("nowhere to nowhere is not zero")
	}
	// Faster ways are faster, and a short trip on transit is a walk.
	if !(Minutes("car", d) < Minutes("transit", d) && Minutes("transit", d) < Minutes("bike", d)+15 && Minutes("bike", d) < Minutes("walk", d)) {
		t.Errorf("walk %d, bike %d, transit %d, car %d", Minutes("walk", d), Minutes("bike", d), Minutes("transit", d), Minutes("car", d))
	}
	if Minutes("transit", 0.4) != Minutes("walk", 0.4) {
		t.Error("400 m by transit should be a walk")
	}
	if Minutes("walk", 0) != 1 {
		t.Error("even no distance takes a minute")
	}
	if CleanMode("rocket") != "transit" || CleanMode("bike") != "bike" {
		t.Error("CleanMode")
	}
}

func TestSuggestPicksTheShortestLongestTrip(t *testing.T) {
	starts := []Start{
		{ID: 1, Name: "Anna", At: schwabing, Mode: "transit"},
		{ID: 2, Name: "Ben", At: giesing, Mode: "transit"},
		{ID: 3, Name: "Chris", At: pasing, Mode: "transit"},
	}
	cands := []Venue{
		{Ref: "node/1", Name: "Next door to Anna", At: Point{48.1640, 11.5870}},
		{Ref: "node/2", Name: "Café Central", At: marienpl},
		{Ref: "node/3", Name: "Out west", At: Point{48.1500, 11.4600}},
		{Ref: "node/4", Name: "Café Central", At: Point{48.1380, 11.5750}}, // a second branch
		{Ref: "node/5", Name: "", At: marienpl},                            // no name, no use
		{Ref: "node/6", Name: "Somewhere else", At: Point{48.14, 11.55}},
	}
	got := Suggest(cands, starts, 3)
	if len(got) != 3 || got[0].Name != "Somewhere else" && got[0].Name != "Café Central" {
		t.Fatalf("got %+v", got)
	}
	names := map[string]int{}
	for _, v := range got {
		names[v.Name]++
	}
	if names["Café Central"] != 1 {
		t.Errorf("a chain should appear once: %+v", got)
	}
	// The middle beats somewhere convenient for one person.
	if !Fairer(Measure(marienpl, starts), Measure(Point{48.1640, 11.5870}, starts)) {
		t.Error("Marienplatz should be fairer than Anna's doorstep")
	}
	f := Measure(marienpl, starts)
	if len(f.Trips) != 3 || f.Longest < f.Trips[0].Minutes || f.Total < f.Longest {
		t.Errorf("fairness: %+v", f)
	}
}

func TestArea(t *testing.T) {
	c, km := Area([]Point{schwabing, giesing, pasing})
	if Km(c, marienpl) > 4 {
		t.Errorf("the middle of the three is %.1f km from Marienplatz", Km(c, marienpl))
	}
	if km < 2 || km > 10 {
		t.Errorf("radius %.1f km", km)
	}
	if _, km := Area([]Point{pasing}); km != 1.2 {
		t.Errorf("one person: radius %.1f, want 1.2", km)
	}
	if !strings.HasPrefix(MapURL(marienpl), "https://www.openstreetmap.org/?mlat=48.1374&mlon=11.5755#map=17/") {
		t.Errorf("map link %q", MapURL(marienpl))
	}
}

func TestLabel(t *testing.T) {
	for _, c := range []struct{ name, display, want string }{
		{"Schwabing", "Schwabing, München, Bayern, 80801, Deutschland", "Schwabing, München"},
		{"", "12, Leopoldstraße, Schwabing, München", "12, Leopoldstraße"},
		{"Ostbahnhof", "Ostbahnhof, 81667, Haidhausen, München", "Ostbahnhof, Haidhausen"},
		{"Nowhere", "", "Nowhere"},
	} {
		if got := label(c.name, c.display); got != c.want {
			t.Errorf("label(%q, %q) = %q, want %q", c.name, c.display, got, c.want)
		}
	}
}

// fakeOSM stands in for Nominatim and Overpass.
type fakeOSM struct {
	mu       sync.Mutex
	requests []*http.Request
	queries  []string
}

func (f *fakeOSM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r)
	f.mu.Unlock()
	switch r.URL.Path {
	case "/search":
		json.NewEncoder(w).Encode([]map[string]any{
			{"lat": "48.1636", "lon": "11.5868", "name": "Schwabing", "display_name": "Schwabing, München, Bayern"},
			{"lat": "nonsense", "lon": "11", "name": "Broken"},
		})
	case "/interpreter":
		r.ParseForm()
		f.mu.Lock()
		f.queries = append(f.queries, r.PostForm.Get("data"))
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"elements": []map[string]any{
			{"type": "node", "id": 1, "lat": 48.1374, "lon": 11.5755, "tags": map[string]string{"name": "Café Central", "addr:street": "Marienplatz", "addr:housenumber": "1"}},
			{"type": "way", "id": 2, "center": map[string]float64{"lat": 48.14, "lon": 11.58}, "tags": map[string]string{"name": "Park Café"}},
			{"type": "node", "id": 3, "lat": 48.1, "lon": 11.5, "tags": map[string]string{}},
		}})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestOSMClient(t *testing.T) {
	fake := &fakeOSM{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	o := NewOSM(srv.URL, srv.URL+"/interpreter", "admin@example.org")
	clock := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	o.now = func() time.Time { return clock }
	ctx := context.Background()

	found, err := o.Search(ctx, "  Schwabing ", []Point{pasing})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Label != "Schwabing, München" || found[0].At != schwabing {
		t.Errorf("found %+v", found)
	}
	req := fake.requests[0]
	if !strings.Contains(req.Header.Get("User-Agent"), "Halfway/") || !strings.Contains(req.Header.Get("User-Agent"), "admin@example.org") {
		t.Errorf("user agent %q", req.Header.Get("User-Agent"))
	}
	if q, _ := url.ParseQuery(req.URL.RawQuery); q.Get("viewbox") == "" || q.Get("q") != "Schwabing" {
		t.Errorf("query %v", req.URL.RawQuery)
	}
	// Asked again, it comes from the cache.
	o.Search(ctx, "Schwabing", []Point{pasing})
	if len(fake.requests) != 1 {
		t.Errorf("%d requests for the same search", len(fake.requests))
	}
	if got, _ := o.Search(ctx, "   ", nil); len(got) != 0 || len(fake.requests) != 1 {
		t.Error("an empty search went out")
	}

	clock = clock.Add(2 * time.Second)
	venues, err := o.Nearby(ctx, "coffee", marienpl, 1.5)
	if err != nil {
		t.Fatal(err)
	}
	if len(venues) != 2 || venues[0].Ref != "node/1" || venues[0].Address != "Marienplatz 1" || venues[1].At != (Point{48.14, 11.58}) {
		t.Errorf("venues %+v", venues)
	}
	if q := fake.queries[0]; !strings.Contains(q, `["amenity"="cafe"]["name"](around:1500,48.13740,11.57550)`) {
		t.Errorf("overpass query %q", q)
	}
}

func TestOSMWaitsASecondBetweenRequests(t *testing.T) {
	fake := &fakeOSM{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	o := NewOSM(srv.URL, "", "")
	start := time.Now()
	o.Search(context.Background(), "one", nil)
	o.Search(context.Background(), "two", nil)
	if time.Since(start) < 900*time.Millisecond {
		t.Errorf("two searches took %v; the policy is one a second", time.Since(start))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := o.Search(ctx, "three", nil); err == nil {
		t.Error("a cancelled search went ahead")
	}
}
