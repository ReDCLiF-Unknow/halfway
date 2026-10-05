#!/usr/bin/env python3
"""Regenerate the screenshots in docs/.

    python tools/screenshots/shoot.py

It starts a Halfway server on a spare port with an empty database and a
stand-in for OpenStreetMap, fills it with a few friends planning a few things, drives headless Chrome over the
DevTools protocol, and writes the PNGs into docs/. Nothing it touches
outlives the run: the database, the browser profile and both processes live
in a temporary directory that is deleted at the end.

Needs Go, Chrome (or set the CHROME environment variable) and the `websockets`
package: python -m pip install websockets

Why DevTools rather than Chrome's own --screenshot flag: that flag ignores
prefers-color-scheme, so the light screenshot comes out as a second copy of
the dark one, and on Windows it clamps the window to about 476px, so a phone
screenshot is silently rendered at the wrong width. Emulation.* controls both
exactly, and Network.setCookie signs us in without the welcome form.
"""

import asyncio
import base64
import http.cookiejar
import http.server
import threading
import json
import os
import re
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timedelta
from pathlib import Path

try:
    import websockets
except ImportError:
    sys.exit("this needs the websockets package: python -m pip install websockets")

REPO = Path(__file__).resolve().parents[2]
DOCS = REPO / "docs"

# name, path, width, height, colour scheme, phone, signed in. "{open}",
# "{decided}" and "{invite}" are filled in once seed() has made them.
SHOTS = [
    ("poll-dark.png", "{open}", 1280, 980, "dark", False, True),
    ("poll-light.png", "{open}", 1280, 980, "light", False, True),
    ("decided-light.png", "{decided}", 1280, 760, "light", False, True),
    ("dashboard-dark.png", "/", 1280, 860, "dark", False, True),
    ("invite-light.png", "{invite}", 1280, 860, "light", False, False),
    ("mobile-dark.png", "{open}", 390, 844, "dark", True, True),
    ("where-light.png", "{open}", 1280, 900, "light", False, True),
]

# Shots that start scrolled to part of the page.
SCROLL = {"where-light.png": "[data-live=places]"}

# A stand-in for OpenStreetMap, so the screenshots never depend on (or
# bother) the real services: a few made-up places around central Munich.
FAKE_PLACES = [
    ("node/1", "Brasserie am Markt", "Viktualienmarkt 3", 48.1351, 11.5763),
    ("node/2", "Wirtshaus Westend", "Ganghoferstraße 12", 48.1352, 11.5390),
    ("node/3", "Trattoria Isar", "Zweibrückenstraße 8", 48.1335, 11.5850),
    ("node/4", "Bistro Maxvorstadt", "Türkenstraße 40", 48.1500, 11.5780),
    ("node/5", "Gasthaus Sendling", "Plinganserstraße 5", 48.1210, 11.5480),
]


class FakeOSM(http.server.BaseHTTPRequestHandler):
    def do_GET(self):  # Nominatim search
        self.reply([{"lat": "48.1374", "lon": "11.5755", "name": "Marienplatz", "display_name": "Marienplatz, München"}])

    def do_POST(self):  # Overpass
        self.rfile.read(int(self.headers.get("Content-Length", 0)))
        self.reply({"elements": [{"type": r.split("/")[0], "id": int(r.split("/")[1]), "lat": lat, "lon": lon,
                                  "tags": {"name": n, "addr:street": a.rsplit(" ", 1)[0], "addr:housenumber": a.rsplit(" ", 1)[1]}}
                                 for r, n, a, lat, lon in FAKE_PLACES]})

    def reply(self, data):
        body = json.dumps(data).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass

CHROMES = [
    os.environ.get("CHROME"),
    r"C:\Program Files\Google\Chrome\Application\chrome.exe",
    r"C:\Program Files (x86)\Google\Chrome\Application\chrome.exe",
    "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
    "google-chrome",
    "chromium",
]


def find_chrome():
    for c in CHROMES:
        if c and (Path(c).exists() or shutil.which(c)):
            return c
    sys.exit("no Chrome found; set CHROME to its path")


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def kill_tree(proc):
    """Stop a process and its children. `go run` builds and then execs the
    server as a child, so killing only the parent would leave it running."""
    if proc.poll() is not None:
        return
    if os.name == "nt":
        subprocess.run(["taskkill", "/T", "/F", "/PID", str(proc.pid)],
                       capture_output=True)
    else:
        proc.terminate()
    try:
        proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        proc.kill()


def wait_for(url, what, tries=60):
    for _ in range(tries):
        try:
            urllib.request.urlopen(url, timeout=1).read()
            return
        except (urllib.error.URLError, ConnectionError, OSError):
            time.sleep(1)
    sys.exit(f"{what} never came up at {url}")


class Person:
    """Somebody using the app through its own forms, with a cookie jar for a
    browser. There is no JSON API: this is the way in."""

    def __init__(self, base, name):
        self.base = base
        self.jar = http.cookiejar.CookieJar()
        self.web = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar))
        self.post("/welcome", name=name)
        self.token = next(c.value for c in self.jar if c.name == "halfway_token")

    def get(self, path):
        with self.web.open(self.base + path) as r:
            return r.geturl(), r.read().decode()

    def post(self, path, **fields):
        data = urllib.parse.urlencode(fields, doseq=True).encode()
        with self.web.open(self.base + path, data=data) as r:
            return r.geturl(), r.read().decode()

    def create(self, title, category, slots, deadline="", quorum="", places=False):
        """Make a poll and return its id, invite path and time ids."""
        url, page = self.post("/polls", title=title, category=category, slot=slots,
                              deadline=deadline, quorum=quorum, places="on" if places else "")
        pid = int(urllib.parse.urlparse(url).path.rsplit("/", 1)[1])
        invite = re.search(r"/i/[A-Za-z0-9_-]{16}", page).group(0)
        ids = list(dict.fromkeys(re.findall(rf"/polls/{pid}/slots/(\d+)/vote", page)))
        return pid, invite, ids

    def answer(self, pid, slot, answer):
        self.post(f"/polls/{pid}/slots/{slot}/vote", answer=str(answer))


YES, MAYBE, NO = 2, 1, 0


def seed(base):
    """Fill an empty database with polls worth photographing, and return
    Alex's key and the paths to show. Times are relative to now, so "in 2
    days" is always right whenever this is run."""
    day0 = datetime.now().replace(hour=0, minute=0, second=0, microsecond=0)
    at = lambda d, h, m=0: (day0 + timedelta(days=d, hours=h, minutes=m)).strftime("%Y-%m-%dT%H:%M")

    alex = Person(base, "Alex Morgan")
    # Alex has used Halfway a while, so is past the "save your sign-in key"
    # reminder a brand new name sees.
    alex.post("/me/token/saved", next="/")
    sam, priya, jonas, mia = (Person(base, n) for n in ("Sam", "Priya", "Jonas", "Mia"))

    def everyone(invite, *people):
        for p in people:
            p.get(invite)

    # Open, with a minimum, and close to it: the one the screenshots are of.
    dinner, dinner_invite, (thu, fri, sat) = alex.create(
        "Friday dinner", "dinner", [at(3, 19, 30), at(4, 19, 30), at(5, 19)], deadline=at(2, 18), quorum="4", places=True)
    everyone(dinner_invite, sam, priya, jonas, mia)
    for who, answers in ((alex, (YES, YES, NO)), (sam, (NO, YES, MAYBE)), (priya, (YES, MAYBE, YES)), (jonas, (YES, NO, YES))):
        for slot, a in zip((thu, fri, sat), answers):
            who.answer(dinner, slot, a)
    # Where everyone is coming from (rounded by the server, shown to nobody),
    # and the places Halfway suggests for them.
    for who, lat, lon, label, mode in ((alex, 48.1636, 11.5868, "Schwabing, München", "transit"),
                                       (sam, 48.1110, 11.5960, "Giesing", "bike"),
                                       (priya, 48.1494, 11.4614, "Pasing", "transit"),
                                       (jonas, 48.1290, 11.6010, "Haidhausen", "walk")):
        who.post(f"/polls/{dinner}/start", lat=lat, lon=lon, label=label, mode=mode)
    _, page = alex.post(f"/polls/{dinner}/venues/suggest")
    venue_ids = list(dict.fromkeys(re.findall(rf"/polls/{dinner}/venues/(\d+)/vote", page)))
    for who in (alex, priya):
        who.post(f"/polls/{dinner}/venues/{venue_ids[0]}/vote")
    sam.post(f"/polls/{dinner}/venues/{venue_ids[1]}/vote")

    # Decided: its quorum was reached.
    games, games_invite, (g1, g2) = sam.create(
        "Board games night", "drinks", [at(6, 20), at(7, 20)], quorum="3")
    everyone(games_invite, alex, priya, mia)
    for who, answers in ((sam, (YES, YES)), (priya, (NO, YES)), (mia, (MAYBE, YES))):
        for slot, a in zip((g1, g2), answers):
            who.answer(games, slot, a)

    # Waiting for Alex to answer.
    coffee, coffee_invite, (c1, c2, c3) = priya.create(
        "Coffee catch-up", "coffee", [at(2, 10), at(2, 15), at(3, 9, 30)])
    everyone(coffee_invite, alex, jonas)
    priya.answer(coffee, c1, YES)
    jonas.answer(coffee, c2, YES)

    # Called off: nobody could make it.
    hike, hike_invite, (h1,) = mia.create("Sunday hike", "hike", [at(8, 9)], quorum="4")
    everyone(hike_invite, alex)
    mia.answer(hike, h1, YES)
    mia.post(f"/polls/{hike}/decide")

    # Alex has seen the board games decision except for the latest.
    alex.get(f"/polls/{hike}")
    return alex.token, {"open": f"/polls/{dinner}", "decided": f"/polls/{games}", "invite": dinner_invite}


def page_target(port):
    with urllib.request.urlopen(f"http://127.0.0.1:{port}/json") as r:
        for t in json.load(r):
            if t["type"] == "page":
                return t["webSocketDebuggerUrl"]
    sys.exit("Chrome exposed no page to drive")


class CDP:
    """Just enough of the DevTools protocol: send a command, await its reply."""

    def __init__(self, ws):
        self.ws, self.n = ws, 0

    async def send(self, method, **params):
        self.n += 1
        await self.ws.send(json.dumps({"id": self.n, "method": method, "params": params}))
        while True:
            msg = json.loads(await self.ws.recv())
            if msg.get("id") == self.n:
                if "error" in msg:
                    sys.exit(f"{method}: {msg['error']}")
                return msg.get("result", {})

    async def wait_for(self, event, timeout=30):
        while True:
            msg = json.loads(await asyncio.wait_for(self.ws.recv(), timeout))
            if msg.get("method") == event:
                return msg


async def capture(debug_port, base, token, paths):
    async with websockets.connect(page_target(debug_port), max_size=64 * 1024 * 1024) as ws:
        cdp = CDP(ws)
        await cdp.send("Page.enable")
        await cdp.send("Network.enable")
        # Sign in without the welcome form. The app's cookie is HttpOnly, which
        # only stops page scripts from reading it, not DevTools from setting it.
        await cdp.send("Network.setCookie", name="halfway_token", value=token,
                       domain="localhost", path="/", httpOnly=True)
        for name, path, w, h, scheme, phone, signed_in in SHOTS:
            await cdp.send("Emulation.setDeviceMetricsOverride", width=w, height=h,
                           deviceScaleFactor=2, mobile=phone, screenWidth=w, screenHeight=h)
            await cdp.send("Emulation.setEmulatedMedia", media="screen",
                           features=[{"name": "prefers-color-scheme", "value": scheme}])
            await cdp.send("Emulation.setTouchEmulationEnabled", enabled=phone, maxTouchPoints=5)
            # The invite is photographed as the newcomer it is sent to sees
            # it: 127.0.0.1 is another site to the browser, so Alex's cookie
            # for localhost does not go with the request.
            origin = base if signed_in else base.replace("localhost", "127.0.0.1")
            await cdp.send("Page.navigate", url=origin + path.format(**paths))
            await cdp.wait_for("Page.loadEventFired")
            if name in SCROLL:
                await cdp.send("Runtime.evaluate", expression=f"document.querySelector('{SCROLL[name]}').scrollIntoView()")
            await asyncio.sleep(1.5)  # let the fonts settle and the event stream open
            shot = await cdp.send("Page.captureScreenshot", format="png", captureBeyondViewport=False)
            (DOCS / name).write_bytes(base64.b64decode(shot["data"]))
            print(f"  {name}  {w}x{h} @2x {scheme}{' phone' if phone else ''}")


def main():
    chrome = find_chrome()
    port, debug_port = free_port(), free_port()
    base = f"http://localhost:{port}"
    work = Path(tempfile.mkdtemp(prefix="halfway-shots-"))
    server = browser = None
    try:
        osm = http.server.ThreadingHTTPServer(("127.0.0.1", 0), FakeOSM)
        threading.Thread(target=osm.serve_forever, daemon=True).start()
        fake = f"http://127.0.0.1:{osm.server_address[1]}"
        print(f"starting a server on {port}")
        server = subprocess.Popen(
            ["go", "run", "./cmd/server", "-addr", f"localhost:{port}", "-db", str(work / "demo.db")],
            cwd=REPO, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            env={**os.environ, "HALFWAY_PLACES": "on", "HALFWAY_NOMINATIM_URL": fake,
                 "HALFWAY_OVERPASS_URL": fake + "/interpreter", "TZ": os.environ.get("TZ", "Europe/Berlin")})
        wait_for(base + "/welcome", "the server")

        print("seeding the demo data")
        token, paths = seed(base)

        print("starting Chrome")
        browser = subprocess.Popen(
            [chrome, "--headless", "--disable-gpu", "--no-sandbox", "--no-first-run",
             "--hide-scrollbars", f"--remote-debugging-port={debug_port}",
             f"--user-data-dir={work / 'profile'}", "about:blank"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        wait_for(f"http://127.0.0.1:{debug_port}/json/version", "Chrome")

        DOCS.mkdir(exist_ok=True)
        print(f"writing to {DOCS}")
        asyncio.run(capture(debug_port, base, token, paths))
        print("done")
    finally:
        for p in (browser, server):
            if p:
                kill_tree(p)
        shutil.rmtree(work, ignore_errors=True)


if __name__ == "__main__":
    main()
