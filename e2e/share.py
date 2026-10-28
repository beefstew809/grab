import json, time
from urllib.parse import urlparse, parse_qs, unquote
from playwright.sync_api import sync_playwright

BASE = "http://localhost:18081"
URL = "https://www.youtube.com/watch?v=AcvoSoUT7EE"
ID = "10530c6fb5070182"  # Go's ItemID(URL) -- JS must compute the same
OTHER = "aaaaaaaaaaaaaaaa"

def item(id, title, status="finished", **kw):
    return {"id": id, "title": title, "url": URL if id == ID else "https://x/" + id, "source": "youtube",
            "status": status, "percent": kw.get("percent", 0), "size": 20_000_000, "timestamp": 1, "exists": True,
            "public": False, "mine": True, "target": "original", "needsCookies": False}

def detail(id, title):
    d = item(id, title)
    d.update(duration=61, presets=[
        {"name": "original", "label": "Original", "state": "ready", "estBytes": 1000},
        {"name": "messages", "label": "Messages", "state": "convert", "estBytes": 9_000_000},
        {"name": "discord", "label": "Discord", "state": "too_long", "estBytes": 0, "maxDuration": 181},
    ])
    return d


PROG = "9999999999999999"     # share -> progress screen -> ready card
DONE = "dddddddddddddddd"     # share of something already downloaded

def progress_api(notify_set, seq):
    """A self-contained mock for the share progress screen; seq is the list of
    item states returned by successive GET /api/items/PROG polls."""
    n = {"i": 0}
    def handler(route):
        req = route.request
        path = urlparse(req.url).path
        if path == "/api/me":
            return route.fulfill(json={"name": "Ada", "dir": "ada", "admin": False, "auth": "oidc",
                "defaultTarget": "signal", "targets": TARGETS,
                "notify": {"set": notify_set, "count": 1 if notify_set else 0, "instanceWide": False}})
        if path == "/api/downloads":
            body = json.loads(req.post_data)
            if "already" in body["url"]:
                return route.fulfill(json={"id": DONE, "existing": True})
            return route.fulfill(status=201, json={"id": PROG})
        if path == f"/api/items/{DONE}":
            return route.fulfill(json=detail(DONE, "Old one"))
        if path == f"/api/items/{PROG}":
            st = seq[min(n["i"], len(seq) - 1)]
            n["i"] += 1
            return route.fulfill(json=st)
        return route.fulfill(status=404, json={"error": "not found"})
    return handler

def prog_item(status, percent=0, title="", presets=None):
    d = item(PROG, title, status=status, percent=percent)
    d.update(url="https://youtu.be/abc", target="signal")
    if presets is not None:
        d.update(duration=30, presets=presets)
    return d

polls = {"n": 0}
NOSLOT = "ffffffffffffffff"   # failed on an unknown host: no cookie slot
ERR = "eeeeeeeeeeeeeeee"      # failed download that wants cookies
LAG = "bbbbbbbbbbbbbbbb"    # finished, but the file shows up on NFS a bit later
FLAKY = "cccccccccccccccc"  # converting; one poll fails mid-conversion
lag = {"n": 0}
flaky = {"n": 0}

def lag_detail():
    lag["n"] += 1
    d = detail(LAG, "Lagging")
    d["timestamp"] = int(time.time() * 1e9)  # queued just now
    if lag["n"] < 3:
        d["exists"] = False
        for p in d["presets"]:
            p["state"] = "missing"
    return d

def flaky_detail():
    flaky["n"] += 1
    d = detail(FLAKY, "Flaky")
    d["presets"][1]["state"] = "converting" if flaky["n"] < 4 else "ready"
    d["presets"][1]["percent"] = 50
    return d

calls = []  # (method, path, body) of every mutating request
# Built at runtime so secret scanners see no token-shaped literal.
MOCK_TOKEN = "grab_" + "x" * 43
state = {"default": "signal", "tokens": [], "cookies": {"youtube": {"updated": "2026-10-01T00:00:00Z"}}}
TARGETS = [{"name": "original", "label": "Download only"}, {"name": "telegram", "label": "Telegram"},
           {"name": "signal", "label": "Signal"}, {"name": "messages", "label": "Messages"},
           {"name": "discord", "label": "Discord"}]

def api(route):
    req = route.request
    url = req.url
    path = urlparse(url).path
    if req.method != "GET":
        calls.append((req.method, path, req.post_data))
    if path == "/api/me" and req.method == "GET":
        return route.fulfill(json={"name": "Ada", "dir": "ada", "admin": False, "auth": "oidc",
            "defaultTarget": state["default"], "targets": TARGETS,
            "notify": {"set": False, "count": 0, "instanceWide": False}})
    if path == "/api/me" and req.method == "PUT":
        state["default"] = json.loads(req.post_data)["defaultTarget"]
        return route.fulfill(status=204)
    if path == "/api/downloads":
        return route.fulfill(status=201, json={"id": ID})
    if path.endswith("/public") or (req.method == "DELETE" and path.startswith("/api/items/")):
        return route.fulfill(status=204)
    if path == "/api/tokens" and req.method == "POST":
        state["tokens"].append({"id": "t1", "label": json.loads(req.post_data)["label"], "created": "2026-10-22T00:00:00Z", "lastUsed": None})
        return route.fulfill(status=201, json={"id": "t1", "label": state["tokens"][0]["label"], "token": MOCK_TOKEN})
    if path == "/api/tokens":
        return route.fulfill(json=state["tokens"])
    if path == "/api/cookies" and state.get("cookies_fail"):
        return route.fulfill(status=500, json={"error": "cannot list cookies"})
    if path == "/api/cookies":
        return route.fulfill(json=state["cookies"])
    if url.endswith("/api/items?limit=50"):
        return route.fulfill(json=[item(OTHER, "<b>bold</b> title"), item(ID, "Tedeschi")])
    if f"/api/items/{ID}/file" in url or f"/api/items/{OTHER}/file" in url:
        return route.fulfill(body=b"\0" * 1000, headers={"Content-Type": "video/mp4", "Content-Length": "1000"})
    if url.endswith(f"/api/items/{ID}") and req.method == "GET":
        polls["n"] += 1
        if polls["n"] < 3:
            return route.fulfill(json=item(ID, "Tedeschi", status="downloading", percent=40))
        return route.fulfill(json=detail(ID, "Tedeschi"))
    if url.endswith(f"/api/items/{ERR}"):
        d = item(ERR, "Private reel", status="error")
        d.update(source="instagram", error="login required", needsCookies=True)
        return route.fulfill(json=d)
    if url.endswith(f"/api/items/{NOSLOT}"):
        d = item(NOSLOT, "Unknown host", status="error")
        d.update(source="other", error="please log in", needsCookies=True)
        return route.fulfill(json=d)
    if url.endswith(f"/api/items/{LAG}"):
        return route.fulfill(json=lag_detail())
    if url.endswith(f"/api/items/{FLAKY}"):
        if flaky["n"] == 1:  # first poll after the card renders: a blip
            flaky["n"] += 1
            return route.fulfill(status=502, json={"error": "downloader unreachable"})
        return route.fulfill(json=flaky_detail())
    if f"/api/items/{FLAKY}/file" in url:
        return route.fulfill(body=b"\0" * 1000, headers={"Content-Type": "video/mp4", "Content-Length": "1000"})
    if url.endswith(f"/api/items/{OTHER}"):
        return route.fulfill(json=detail(OTHER, "<b>bold</b> title"))
    return route.fulfill(status=404, json={"error": "not found"})

with sync_playwright() as p:
    b = p.chromium.launch()
    ctx = b.new_context(viewport={"width": 412, "height": 800})
    ctx.add_init_script("""
      navigator.canShare = () => true;
      navigator.share = async (d) => { window.__shared = {n: d.files.length, type: d.files[0].type,
        name: d.files[0].name, size: d.files[0].size, text: d.text ?? null}; };
    """)
    ctx.route("**/api/**", api)
    pg = ctx.new_page()

    # Recent list: titles render as TEXT, never HTML.
    pg.goto(BASE + "/")
    pg.wait_for_selector("ul.recent li a")
    assert pg.locator("ul.recent b").count() == 0, "title HTML was rendered"
    assert "<b>bold</b> title" in pg.locator("ul.recent").inner_text()

    # Share card: too_long disabled with the max duration; Get -> Share hands over ONE mp4.
    pg.locator("ul.recent li a").first.click()
    pg.wait_for_selector(".preset")
    discord = pg.locator(".preset", has_text="Discord")
    assert discord.locator("button").is_disabled()
    assert "3:01" in discord.inner_text(), discord.inner_text()
    orig = pg.locator(".preset", has_text="Original")
    orig.locator("button").click()
    pg.wait_for_function("document.querySelector('.preset button').textContent === 'Share'")
    orig.locator("button").click()
    pg.wait_for_function("window.__shared")
    s = pg.evaluate("window.__shared")
    # fileName() turns filename-illegal characters (incl. < > /) into spaces.
    assert s == {"n": 1, "type": "video/mp4", "name": "b bold b title.mp4", "size": 1000, "text": None}, s

    # Chrome on Android refuses (NotAllowedError) a shared file named
    # "He Impressed Frieza....mp4" -- found with /diag.html. Dot runs collapse
    # and trailing dots go before the extension is added.
    for t, want in [("He Impressed Frieza...", "He Impressed Frieza.mp4"), ("a..b  c.", "a.b c.mp4")]:
        assert pg.evaluate("t => fileName({title: t})", t) == want, (t, pg.evaluate("t => fileName({title: t})", t))

    # Include link toggle (off by default) adds the source URL as text.
    pg.evaluate("window.__shared = null")
    assert not pg.locator("#link").is_checked()
    pg.locator("#link").check()
    orig.locator("button").click()
    pg.wait_for_function("window.__shared")
    assert pg.evaluate("window.__shared.text") == "https://x/" + OTHER

    # Ingest: share-target GET -> POST /api/downloads -> live card -> share card.
    pg.goto(BASE + "/?text=" + URL)
    pg.wait_for_selector(".preset", timeout=15000)
    assert ID in pg.url, pg.url
    assert "Tedeschi" in pg.locator("h2").inner_text()
    pg.screenshot(path="/out/share_card.png")

    # NFS lag: finished but file not visible yet -> keep waiting, never a dead "File missing" card.
    pg.goto(BASE + "/?item=" + LAG)
    pg.wait_for_function("document.querySelector('.preset button') && document.querySelector('.preset button').textContent === 'Get'", timeout=15000)

    # One failed poll mid-conversion must not freeze the row.
    pg.goto(BASE + "/?item=" + FLAKY)
    pg.wait_for_function("[...document.querySelectorAll('.preset')].some(r => r.textContent.includes('Messages') && r.querySelector('button').textContent === 'Share')", timeout=20000)

    # test_footer_links_source: AGPL section 13 -- every page offers the source.
    fc = b.new_context(viewport={"width": 412, "height": 800})
    fc.route("**/api/**", api)
    fp = fc.new_page()
    fp.goto(BASE + "/")
    src = fp.get_by_role("link", name="Source code")
    assert src.get_attribute("href") == "https://github.com/beefstew809/grab", src.get_attribute("href")
    fc.close()

    # test_share_shows_progress_screen: a share says what is happening at once,
    # then walks the stages until the finished card replaces it.
    seq = [prog_item("waiting"), prog_item("downloading", 42, "Clip"),
           prog_item("finished", 100, "Clip", [{"name": "signal", "label": "Signal", "state": "converting", "percent": 30, "estBytes": 1}]),
           prog_item("finished", 100, "Clip", [{"name": "signal", "label": "Signal", "state": "ready", "estBytes": 1000}])]
    pc = b.new_context(viewport={"width": 412, "height": 800})
    pc.route("**/api/**", progress_api(False, seq))
    pp = pc.new_page()
    pp.goto(BASE + "/?text=look%20https://youtu.be/abc")
    pp.wait_for_selector("text=Got it — downloading")
    assert "youtu.be/abc" in pp.inner_text("#main"), pp.inner_text("#main")
    assert "Recent list" in pp.inner_text("#main")  # no notifications set up
    pp.wait_for_selector("text=Downloading 42%")
    assert "Then: fit for Signal" in pp.inner_text("#main")
    pp.wait_for_selector("text=Fitting for Signal 30%")
    pp.wait_for_selector(".preset")  # ready: the normal item card takes over
    assert "Got it" not in pp.inner_text("#main")
    pc.close()

    # test_progress_hint_with_notifications: with notifications on, it says you can close it.
    nc = b.new_context(viewport={"width": 412, "height": 800})
    nc.route("**/api/**", progress_api(True, [prog_item("waiting")]))
    np_ = nc.new_page()
    np_.goto(BASE + "/?url=https%3A%2F%2Fyoutu.be%2Fabc")
    np_.wait_for_selector("text=Got it — downloading")
    assert "notification" in np_.inner_text("#main"), np_.inner_text("#main")
    nc.close()

    # test_share_existing_says_already_downloaded
    ec = b.new_context(viewport={"width": 412, "height": 800})
    ec.route("**/api/**", progress_api(False, []))
    ep = ec.new_page()
    ep.goto(BASE + "/?url=https%3A%2F%2Fyoutu.be%2Falready")
    ep.wait_for_selector("text=Already downloaded")
    ep.wait_for_selector(".preset")
    ec.close()

    # test_logged_out_share_redirects_and_returns: the shared link survives the login round trip.
    lo = b.new_context(viewport={"width": 412, "height": 800})
    lo.route("**/api/**", lambda r: r.fulfill(status=401, json={"error": "login required"}))
    lp = lo.new_page()
    lp.goto(BASE + "/?url=" + "https%3A%2F%2Fyoutu.be%2Fabc%3Ft%3D1")
    lp.wait_for_url("**/auth/login?*")
    ret = parse_qs(urlparse(lp.url).query)["return"][0]
    assert ret == "/?url=https%3A%2F%2Fyoutu.be%2Fabc%3Ft%3D1", ret
    assert "return=%2F%3Furl%3D" in lp.url, lp.url
    assert parse_qs(urlparse(ret).query)["url"][0] == "https://youtu.be/abc?t=1"
    lo.close()

    # test_share_submits_with_default_target: words around the link are ignored.
    calls.clear()
    sp = ctx.new_page()
    sp.goto(BASE + "/?text=look%20https://youtu.be/abc")
    sp.wait_for_selector(".preset", timeout=15000)
    posts = [c for c in calls if c[1] == "/api/downloads"]
    assert [json.loads(c[2]) for c in posts] == [{"url": "https://youtu.be/abc", "target": "signal"}], posts
    sp.close()

    # test_paste_with_target_button: the default is highlighted; a button submits its own target.
    calls.clear()
    tp = ctx.new_page()
    tp.goto(BASE + "/")
    tp.wait_for_selector(".targets button")
    assert tp.locator(".targets button").all_inner_texts() == ["Download only", "Telegram", "Signal", "Messages", "Discord"]
    assert tp.locator(".targets button.default").inner_text() == "Signal"
    tp.fill("#u", "https://youtu.be/xyz")
    tp.locator(".targets button", has_text="Discord").click()
    tp.wait_for_selector(".preset", timeout=15000)
    assert [json.loads(c[2]) for c in calls if c[1] == "/api/downloads"] == [{"url": "https://youtu.be/xyz", "target": "discord"}], calls
    # Enter uses the default.
    calls.clear()
    tp.goto(BASE + "/")
    tp.fill("#u", "https://youtu.be/enter")
    tp.press("#u", "Enter")
    tp.wait_for_selector(".preset", timeout=15000)
    assert [json.loads(c[2]) for c in calls if c[1] == "/api/downloads"] == [{"url": "https://youtu.be/enter", "target": "signal"}], calls
    tp.close()

    # test_public_toggle_and_delete
    calls.clear()
    pp = ctx.new_page()
    pp.goto(BASE + "/?item=" + OTHER)
    pp.wait_for_selector(".owner")
    with pp.expect_request("**/public"):
        pp.locator(".owner input[type=checkbox]").check()
    assert ("POST", f"/api/items/{OTHER}/public", '{"public":true}') in calls, calls
    pp.once("dialog", lambda d: d.accept())
    pp.locator(".owner button", has_text="Delete").click()
    pp.wait_for_selector("ul.recent")
    assert ("DELETE", f"/api/items/{OTHER}", None) in calls, calls
    pp.close()

    # test_cookie_hint_on_error: link to the cookies row, which scrolls into view and is highlighted.
    cp2 = ctx.new_page()
    cp2.goto(BASE + "/?item=" + ERR)
    a = cp2.locator("a", has_text="Add instagram cookies")
    a.wait_for()
    assert a.get_attribute("href") == "/?settings#cookies-instagram", a.get_attribute("href")
    a.click()
    cp2.wait_for_selector("#cookies-instagram.target")
    assert "Export cookies" in cp2.locator("section", has_text="Cookies").inner_text()
    cp2.close()

    # No cookie slot for "other": plain error, no dead link.
    ns = ctx.new_page()
    ns.goto(BASE + "/?item=" + NOSLOT)
    ns.wait_for_selector(".banner >> text=Download failed")
    assert "please log in" in ns.locator("main").inner_text()
    assert ns.locator("a", has_text="cookies").count() == 0
    ns.close()

    # One failing section must not blank the settings page.
    state["cookies_fail"] = True
    fs = ctx.new_page()
    fs.goto(BASE + "/?settings")
    fs.wait_for_selector("#default-target")
    fs.wait_for_selector("section >> text=Notifications")
    fs.wait_for_selector(".banner >> text=cannot list cookies")
    fs.close()
    state["cookies_fail"] = False

    # test_settings_tokens_show_once
    tk = ctx.new_page()
    tk.goto(BASE + "/?settings")
    tk.wait_for_selector("#token-label")
    # The folder name, which the owner needs for `grab migrate --user`.
    assert tk.locator("#user-dir").inner_text() == "ada", tk.locator("#user-dir").inner_text()
    tk.fill("#token-label", "phone")
    tk.get_by_role("button", name="Create token").click()
    tk.wait_for_selector("#new-token")
    assert tk.locator("#new-token").inner_text() == MOCK_TOKEN
    tk.reload()
    tk.wait_for_selector("#token-label")
    tk.wait_for_selector("ul.plain li >> text=phone")
    assert MOCK_TOKEN not in tk.content()
    assert tk.locator("#new-token").count() == 0
    # Default target select saves.
    tk.select_option("#default-target", "telegram")
    tk.wait_for_function("document.body.textContent.includes('Saved ✓')")
    assert state["default"] == "telegram"
    tk.close()

    # Firefox: can't share files at all -> Download link straight away, and the
    # file is never pulled into page memory.
    fx = b.new_context(viewport={"width": 412, "height": 800})
    fx.add_init_script("navigator.canShare = () => false;")
    fetched = []
    def fx_api(route):
        if "/file" in route.request.url:
            fetched.append(route.request.url)
        return api(route)
    fx.route("**/api/**", fx_api)
    fp = fx.new_page()
    fp.goto(BASE + "/?item=" + OTHER)
    fp.wait_for_selector(".preset a.download")
    dl = fp.locator(".preset", has_text="Original").locator("a.download")
    assert dl.get_attribute("href") == f"/api/items/{OTHER}/file?preset=original&download=1", dl.get_attribute("href")
    assert not fetched, fetched
    fx.close()

    # Chrome refuses the share (e.g. over its size limit): offer Download.
    cr = b.new_context(viewport={"width": 412, "height": 800})
    cr.add_init_script("""
      navigator.canShare = () => true;
      navigator.share = async () => { throw new DOMException('Permission denied', 'NotAllowedError'); };
    """)
    cr.route("**/api/**", api)
    cp = cr.new_page()
    cp.goto(BASE + "/?item=" + OTHER)
    row = cp.locator(".preset", has_text="Original")
    row.locator("button").click()
    cp.wait_for_function("document.querySelector('.preset button').textContent === 'Share'")
    row.locator("button").click()
    cp.wait_for_selector(".preset a.download")
    assert "download" in row.inner_text().lower(), row.inner_text()
    assert row.locator("a.download").get_attribute("href").endswith("&download=1")
    cr.close()

    print("PASS")
    b.close()
