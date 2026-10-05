package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	// Time zones compiled in, so TZ=Europe/Berlin works in a container that
	// has no zone files of its own. Every time on a poll is in this zone.
	_ "time/tzdata"

	"halfway/internal/places"
	"halfway/internal/store"
	"halfway/internal/telegram"
	"halfway/internal/web"
)

func main() {
	addr := flag.String("addr", "localhost:8080", "address to listen on")
	dbPath := flag.String("db", "halfway.db", "path to the SQLite database file")
	flag.Parse()

	// Whether the database is there already has to be known before opening
	// it, which creates it when it is not.
	_, statErr := os.Stat(*dbPath)
	existed := statErr == nil

	s, err := store.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close() // only after serve has returned, so no request is still using it
	users, err := s.UserCount()
	if err != nil {
		log.Print(err)
		return
	}
	log.Print(describeDB(*dbPath, existed, users))
	log.Printf("times are in %s", time.Local)

	// Ctrl-C, or the TERM a container runtime sends.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	bot := startTelegram(ctx, s)
	var opts []web.Option
	if strings.EqualFold(os.Getenv("HALFWAY_DISCORD"), "off") {
		opts = append(opts, web.WithDiscord(nil))
		log.Print("discord: off")
	}
	app := web.New(s, bot, startPlaces(), opts...)
	go app.Run(ctx)

	srv := &http.Server{
		Handler: app,
		// Enough that nobody can hold a connection open by dribbling out a
		// request, without cutting anyone off mid-request.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout on purpose: it covers the whole response, and a
		// server-sent event stream is a response that never ends.
	}
	// Those streams never finish by themselves, so shutting down ends them
	// explicitly; every other request is left to finish.
	srv.RegisterOnShutdown(app.CloseStreams)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("listening on http://%s (db: %s)", *addr, *dbPath)
	if err := serve(ctx, srv, ln); err != nil {
		log.Print(err)
		return // through the deferred Close, which log.Fatal would skip
	}
	log.Print("stopped")
}

// startTelegram connects the Telegram bot when HALFWAY_TELEGRAM_TOKEN is set,
// and returns nil otherwise: then nothing ever leaves the server.
func startTelegram(ctx context.Context, s *store.Store) *telegram.Bot {
	token := os.Getenv("HALFWAY_TELEGRAM_TOKEN")
	if token == "" {
		log.Print("telegram: off (set HALFWAY_TELEGRAM_TOKEN to post decisions into groups)")
		return nil
	}
	bot := telegram.New(token, os.Getenv("HALFWAY_TELEGRAM_API"))
	start, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := bot.Start(start); err != nil {
		log.Printf("telegram: off, the token did not work: %v", err)
		return nil
	}
	log.Printf("telegram: on, as @%s", bot.Username)
	go bot.Listen(ctx, s)
	return bot
}

// startPlaces turns on finding places to meet when HALFWAY_PLACES is on, and
// returns nil otherwise. Searches go from this server to OpenStreetMap's
// Nominatim and Overpass (or the ones HALFWAY_NOMINATIM_URL and
// HALFWAY_OVERPASS_URL name), never from people's browsers.
func startPlaces() places.Finder {
	switch strings.ToLower(os.Getenv("HALFWAY_PLACES")) {
	case "on", "1", "true", "yes":
	default:
		log.Print("places: off (set HALFWAY_PLACES=on to suggest places to meet)")
		return nil
	}
	nominatim, overpass := os.Getenv("HALFWAY_NOMINATIM_URL"), os.Getenv("HALFWAY_OVERPASS_URL")
	finder := places.NewOSM(nominatim, overpass, os.Getenv("HALFWAY_PLACES_CONTACT"))
	if nominatim == "" {
		nominatim = places.DefaultNominatim
	}
	if overpass == "" {
		overpass = places.DefaultOverpass
	}
	log.Printf("places: on, searching %s and %s", nominatim, overpass)
	return finder
}

// describeDB says which database the server is using and what is in it.
//
// Every poll lives in that one file, so a server that comes up on a new,
// empty one where it used to have one has lost every poll, and every invite
// link made before now leads nowhere. That is what happens when a container's
// /data is not on a volume that outlasts a redeploy, and the app itself cannot
// tell it apart from a first run. So a new database is announced loudly,
// where whoever runs the server looks first.
func describeDB(path string, existed bool, users int) string {
	if !existed {
		return "database: created a new, empty one at " + path + `

    If this is not the first time this server has started, its polls are not
    where it is looking, and every link made before now will lead nowhere.
    Keep the database on storage that outlasts a restart: with Docker, a
    volume for /data (docker run -v halfway:/data ...), and on a hosting
    platform, its persistent storage mounted at /data.
`
	}
	return fmt.Sprintf("database: %s, %s", path, plural(users, "person", "people"))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// serve runs srv on ln until ctx is cancelled, then shuts it down and returns
// only once the requests already in progress have finished, or ten seconds
// have passed. Returning any sooner would let main close the database under
// requests that are still using it.
func serve(ctx context.Context, srv *http.Server, ln net.Listener) error {
	drained := make(chan error, 1)
	go func() {
		<-ctx.Done()
		log.Print("shutting down")
		quit, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		drained <- srv.Shutdown(quit)
	}()

	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// Serve returns as soon as Shutdown closes the listener. The requests that
	// were running are only done when Shutdown itself returns.
	if err := <-drained; err != nil {
		log.Printf("some requests were cut short: %v", err)
	}
	return nil
}
