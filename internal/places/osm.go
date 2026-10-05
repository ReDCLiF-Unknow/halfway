package places

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Finder looks places up: where someone sets off from, and what there is to
// meet at around a point.
type Finder interface {
	// Search finds places matching what someone typed ("Schwabing",
	// "Ostbahnhof"), nearest to the points given first.
	Search(ctx context.Context, q string, near []Point) ([]Place, error)
	// Nearby finds venues of a kind of meetup within km of center.
	Nearby(ctx context.Context, category string, center Point, km float64) ([]Venue, error)
}

// Place is a search result: a name to show, and where it is.
type Place struct {
	Label string `json:"label"`
	At    Point  `json:"at"`
}

// The public OpenStreetMap services, which anyone may use within their
// usage policies: an identifying user agent, at most one request a second,
// and results cached rather than asked for again.
const (
	DefaultNominatim = "https://nominatim.openstreetmap.org"
	DefaultOverpass  = "https://overpass-api.de/api/interpreter"
)

// OSM finds places in OpenStreetMap's data: Nominatim for searching,
// Overpass for what there is around a point.
type OSM struct {
	nominatim, overpass string
	agent               string
	http                *http.Client

	mu    sync.Mutex
	last  map[string]time.Time // per service, for one request a second
	cache map[string]cached
	now   func() time.Time
}

type cached struct {
	at    time.Time
	value any
}

// cacheFor is how long an answer is kept. Cafés do not move in a day.
const cacheFor = 24 * time.Hour

// NewOSM uses the services at nominatim and overpass (the public ones when
// empty). contact, an email address or URL, goes in the user agent, as the
// services ask of anyone sending them more than a little.
func NewOSM(nominatim, overpass, contact string) *OSM {
	if nominatim == "" {
		nominatim = DefaultNominatim
	}
	if overpass == "" {
		overpass = DefaultOverpass
	}
	agent := "Halfway/1.1 (+https://github.com/ReDCLiF-Unknow/halfway)"
	if contact != "" {
		agent = "Halfway/1.1 (" + contact + "; +https://github.com/ReDCLiF-Unknow/halfway)"
	}
	return &OSM{
		nominatim: strings.TrimRight(nominatim, "/"), overpass: overpass, agent: agent,
		http: &http.Client{Timeout: 25 * time.Second},
		last: map[string]time.Time{}, cache: map[string]cached{}, now: time.Now,
	}
}

// remembered is a cached answer for key, if there is a fresh one.
func (o *OSM) remembered(key string) (any, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	c, ok := o.cache[key]
	if !ok || o.now().Sub(c.at) > cacheFor {
		return nil, false
	}
	return c.value, true
}

func (o *OSM) remember(key string, v any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.cache) >= 1000 {
		o.cache = map[string]cached{} // crude, but bounded
	}
	o.cache[key] = cached{at: o.now(), value: v}
}

// turn waits until a request to service is allowed: one a second.
func (o *OSM) turn(ctx context.Context, service string) error {
	for {
		o.mu.Lock()
		wait := time.Second - o.now().Sub(o.last[service])
		if wait <= 0 {
			o.last[service] = o.now()
			o.mu.Unlock()
			return nil
		}
		o.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

func (o *OSM) fetch(ctx context.Context, service string, req *http.Request, out any) error {
	if err := o.turn(ctx, service); err != nil {
		return err
	}
	req.Header.Set("User-Agent", o.agent)
	req.Header.Set("Accept", "application/json")
	resp, err := o.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", service, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}
	return nil
}

// Search asks Nominatim, leaning towards the points given, if any.
func (o *OSM) Search(ctx context.Context, q string, near []Point) ([]Place, error) {
	q = strings.Join(strings.Fields(q), " ")
	if q == "" {
		return []Place{}, nil
	}
	v := url.Values{"q": {q}, "format": {"jsonv2"}, "limit": {"6"}}
	if len(near) > 0 {
		c, _ := Area(near)
		// A box of roughly 40 km around everyone so far, as a preference:
		// "Ostbahnhof" means the one in your city, not the first in the world.
		v.Set("viewbox", fmt.Sprintf("%.4f,%.4f,%.4f,%.4f", c.Lon-0.3, c.Lat+0.2, c.Lon+0.3, c.Lat-0.2))
	}
	key := "search?" + v.Encode()
	if hit, ok := o.remembered(key); ok {
		return hit.([]Place), nil
	}
	req, err := http.NewRequestWithContext(ctx, "GET", o.nominatim+"/search?"+v.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var found []struct {
		Lat, Lon    string
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
	}
	if err := o.fetch(ctx, "nominatim", req, &found); err != nil {
		return nil, err
	}
	out := []Place{}
	for _, f := range found {
		lat, err1 := strconv.ParseFloat(f.Lat, 64)
		lon, err2 := strconv.ParseFloat(f.Lon, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, Place{Label: label(f.Name, f.DisplayName), At: Point{lat, lon}})
	}
	o.remember(key, out)
	return out, nil
}

// label shortens Nominatim's long names to something a person reads:
// "Schwabing, Munich" rather than five lines of districts and postcodes.
func label(name, display string) string {
	parts := strings.Split(display, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	if name == "" && len(parts) > 0 {
		name = parts[0]
		parts = parts[1:]
	} else if len(parts) > 0 && parts[0] == name {
		parts = parts[1:]
	}
	// The next part that is a name, not a house number or a postcode.
	for _, p := range parts {
		if p != "" && p != name && (p[0] < '0' || p[0] > '9') {
			return name + ", " + p
		}
	}
	return name
}

// overpassFilters is what each kind of meetup looks for in OpenStreetMap.
var overpassFilters = map[string][]string{
	"coffee": {`["amenity"="cafe"]`},
	"dinner": {`["amenity"="restaurant"]`},
	"drinks": {`["amenity"~"^(bar|pub|biergarten)$"]`},
	"hike":   {`["leisure"~"^(park|nature_reserve)$"]`},
	"other":  {`["amenity"~"^(cafe|restaurant|bar|pub)$"]`},
}

// Nearby asks Overpass for named venues of the right kind around center.
func (o *OSM) Nearby(ctx context.Context, category string, center Point, km float64) ([]Venue, error) {
	filters, ok := overpassFilters[category]
	if !ok {
		filters = overpassFilters["other"]
	}
	around := fmt.Sprintf("(around:%d,%.5f,%.5f)", int(km*1000), center.Lat, center.Lon)
	var q strings.Builder
	q.WriteString("[out:json][timeout:20];(")
	for _, f := range filters {
		q.WriteString(`nwr` + f + `["name"]` + around + `;`)
	}
	q.WriteString(");out center 250;")
	key := "nearby?" + q.String()
	if hit, ok := o.remembered(key); ok {
		return hit.([]Venue), nil
	}
	req, err := http.NewRequestWithContext(ctx, "POST", o.overpass, strings.NewReader(url.Values{"data": {q.String()}}.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var res struct {
		Elements []struct {
			Type   string            `json:"type"`
			ID     int64             `json:"id"`
			Lat    float64           `json:"lat"`
			Lon    float64           `json:"lon"`
			Center *Point            `json:"center"`
			Tags   map[string]string `json:"tags"`
		} `json:"elements"`
	}
	if err := o.fetch(ctx, "overpass", req, &res); err != nil {
		return nil, err
	}
	out := []Venue{}
	for _, e := range res.Elements {
		at := Point{e.Lat, e.Lon}
		if e.Center != nil {
			at = *e.Center
		}
		name := strings.TrimSpace(e.Tags["name"])
		if name == "" || !at.Valid() {
			continue
		}
		out = append(out, Venue{
			Ref:     e.Type + "/" + strconv.FormatInt(e.ID, 10),
			Name:    name,
			Address: strings.TrimSpace(e.Tags["addr:street"] + " " + e.Tags["addr:housenumber"]),
			At:      at,
		})
	}
	o.remember(key, out)
	return out, nil
}

// ErrNoFinder is what asking for places gets when the server has them off.
var ErrNoFinder = errors.New("places are not turned on")
