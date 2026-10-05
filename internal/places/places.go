// Package places finds somewhere to meet that is fair to everyone: the
// place whose longest trip is shortest, rather than the middle of a map,
// which can as easily be a lake or a motorway junction.
//
// Travel times are estimates from the distance and how someone travels,
// worked out here with no routing service. They are good enough to tell a
// fair place from an unfair one, and are always shown as "about".
package places

import (
	"math"
	"slices"
	"strconv"
	"strings"
)

// Point is somewhere on the map, in degrees.
type Point struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// Valid reports whether p is somewhere on Earth.
func (p Point) Valid() bool {
	return p.Lat >= -90 && p.Lat <= 90 && p.Lon >= -180 && p.Lon <= 180 && !(p.Lat == 0 && p.Lon == 0)
}

// grid is how coarsely starting points are kept, in degrees: about 550 m
// north to south, and less east to west away from the equator. Nothing more
// precise than this is ever stored.
const grid = 0.005

// Snap rounds a point to the grid, so nobody's starting point is kept more
// precisely than "around here".
func Snap(p Point) Point {
	r := func(v float64) float64 { return math.Round(v/grid) * grid }
	return Point{Lat: math.Round(r(p.Lat)*1e6) / 1e6, Lon: math.Round(r(p.Lon)*1e6) / 1e6}
}

// Km is the distance between two points along the Earth's surface.
func Km(a, b Point) float64 {
	const r = 6371.0
	rad := math.Pi / 180
	dLat, dLon := (b.Lat-a.Lat)*rad, (b.Lon-a.Lon)*rad
	h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(a.Lat*rad)*math.Cos(b.Lat*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * r * math.Asin(math.Min(1, math.Sqrt(h)))
}

// Ways of getting there, in the order the form offers them.
var Modes = []string{"walk", "bike", "transit", "car"}

// CleanMode is mode if it is one of Modes, and transit otherwise: in a city,
// the likeliest way to get anywhere.
func CleanMode(mode string) string {
	if slices.Contains(Modes, mode) {
		return mode
	}
	return "transit"
}

// Minutes estimates how long a trip of km takes by mode. Streets are never
// straight, so the distance is stretched by a detour factor; transit and car
// add the time spent getting to a stop or finding a space.
func Minutes(mode string, km float64) int {
	var m float64
	switch mode {
	case "walk":
		m = km * 1.3 / 4.8 * 60
	case "bike":
		m = km*1.25/15*60 + 2
	case "car":
		m = km*1.35/28*60 + 5
	default: // transit: a short trip is a walk
		walk := km * 1.3 / 4.8 * 60
		m = math.Min(walk, km*1.3/22*60+10)
	}
	return max(1, int(math.Ceil(m)))
}

// Start is where one person sets off from, and how.
type Start struct {
	ID   int64 // whose it is
	Name string
	At   Point
	Mode string
	// Owed is how many minutes more than their share this person has
	// travelled to their group's earlier meetups (less, if negative). Their
	// trips count for more by half of that, up to MaxOwed, so the places a
	// group meets at even out over time.
	Owed float64
}

// MaxOwed is the most a person's past travelling changes how their trip
// counts, in minutes either way: enough to tip a close call, never enough
// to send everyone across town.
const MaxOwed = 15

// weight is how much longer (or shorter) a trip counts for someone owed.
func (s Start) weight() float64 { return math.Max(-MaxOwed, math.Min(MaxOwed, s.Owed/2)) }

// Trip is one person's estimated journey to a place.
type Trip struct {
	ID      int64
	Name    string
	Mode    string
	Minutes int
}

// Venue is a place that could be the one.
type Venue struct {
	Ref     string // "node/123" for OpenStreetMap, empty for one somebody typed in
	Name    string
	Address string
	At      Point
}

// Fairness is how a place treats everyone: each trip, the longest, and all
// of them added up. Score and ScoreTotal are the same, with the trips of
// people owed by their group counted for more; places are ranked by those.
type Fairness struct {
	Trips      []Trip
	Longest    int
	Total      int
	Score      float64
	ScoreTotal float64
}

// Measure works out everyone's trip to at.
func Measure(at Point, starts []Start) Fairness {
	var f Fairness
	for _, s := range starts {
		t := Trip{ID: s.ID, Name: s.Name, Mode: s.Mode, Minutes: Minutes(s.Mode, Km(s.At, at))}
		f.Trips = append(f.Trips, t)
		f.Longest = max(f.Longest, t.Minutes)
		f.Total += t.Minutes
		counted := float64(t.Minutes) + s.weight()
		f.Score = math.Max(f.Score, counted)
		f.ScoreTotal += counted
	}
	return f
}

// Fairer reports whether a treats people better than b: a shorter longest
// trip, then less travelling overall, both counted with what people are owed.
func Fairer(a, b Fairness) bool {
	if a.Score != b.Score {
		return a.Score < b.Score
	}
	return a.ScoreTotal < b.ScoreTotal
}

// Suggest picks the n fairest of the candidates for people setting off from
// starts, fairest first, each name once (a chain has many branches, and
// three of the same café is no choice).
func Suggest(candidates []Venue, starts []Start, n int) []Venue {
	type scored struct {
		v Venue
		f Fairness
	}
	var all []scored
	for _, v := range candidates {
		if strings.TrimSpace(v.Name) != "" && v.At.Valid() {
			all = append(all, scored{v, Measure(v.At, starts)})
		}
	}
	slices.SortStableFunc(all, func(a, b scored) int {
		switch {
		case Fairer(a.f, b.f):
			return -1
		case Fairer(b.f, a.f):
			return 1
		}
		return strings.Compare(a.v.Name, b.v.Name)
	})
	var out []Venue
	seen := map[string]bool{}
	for _, s := range all {
		key := strings.ToLower(strings.TrimSpace(s.v.Name))
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, s.v)
		if len(out) == n {
			break
		}
	}
	return out
}

// Area is where to look for places for people setting off from points: the
// middle of the box around them, and far enough out to reach the edge of the
// spread, within limits. One person is looked around within a short walk.
func Area(points []Point) (Point, float64) {
	if len(points) == 0 {
		return Point{}, 0
	}
	minLat, maxLat, minLon, maxLon := points[0].Lat, points[0].Lat, points[0].Lon, points[0].Lon
	for _, p := range points[1:] {
		minLat, maxLat = math.Min(minLat, p.Lat), math.Max(maxLat, p.Lat)
		minLon, maxLon = math.Min(minLon, p.Lon), math.Max(maxLon, p.Lon)
	}
	center := Point{Lat: (minLat + maxLat) / 2, Lon: (minLon + maxLon) / 2}
	far := 0.0
	for _, p := range points {
		far = math.Max(far, Km(center, p))
	}
	return center, math.Min(10, math.Max(1.2, far*0.6))
}

// MapURL is a link to the place on OpenStreetMap. It is only ever a link a
// person may follow, never something a page loads.
func MapURL(p Point) string {
	return "https://www.openstreetmap.org/?mlat=" + ftoa(p.Lat) + "&mlon=" + ftoa(p.Lon) + "#map=17/" + ftoa(p.Lat) + "/" + ftoa(p.Lon)
}

func ftoa(f float64) string {
	return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(f, 'f', 5, 64), "0"), ".")
}
