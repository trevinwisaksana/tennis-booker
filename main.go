// booker: auto-books courts at eliteclub.saraga.id at the 06:00 WIB release.
package main

import (
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/smtp"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

var baseURL = envOr("BOOKER_BASE_URL", "https://eliteclub.saraga.id")

const (
	slug = "elite-club-epicentrum"
	ua   = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"
)

var (
	wib        = time.FixedZone("WIB", 7*3600)
	courtNames = map[string]string{"371": "Stadium Tennis 1", "372": "Indoor 2", "373": "Indoor 3"}
	errDead    = errors.New("session dead (redirected to /login)")
	csrfRe     = regexp.MustCompile(`<meta\s+name="csrf-token"\s+content="([^"]+)"`)
	formRe     = regexp.MustCompile(`(?is)<form[^>]*action="[^"]*process_booking[^"]*"[^>]*>(.*?)</form>`)
	inputRe    = regexp.MustCompile(`(?is)<input\b([^>]*)>`)
	attrRe     = regexp.MustCompile(`([a-zA-Z_:-]+)\s*=\s*"([^"]*)"`)
)

type config struct {
	label      string
	cookieFile string
	cookieRaw  string
	startTime  string
	courts     []string
	smtpUser   string
	smtpPass   string
	emailTo    string
	selftest   bool
	dryRun     bool
	oneshot    bool
	targetDate string
	fireAt     time.Time
	retryFor   time.Duration
	logDir     string
}

func loadConfig() (*config, error) {
	c := &config{
		label:      envOr("BOOKER_LABEL", "booker"),
		cookieFile: os.Getenv("BOOKER_COOKIE_FILE"),
		cookieRaw:  strings.TrimSpace(os.Getenv("BOOKER_COOKIE")),
		startTime:  envOr("BOOKER_START_TIME", "06:00"),
		smtpUser:   os.Getenv("BOOKER_SMTP_USER"),
		smtpPass:   os.Getenv("BOOKER_SMTP_PASS"),
		emailTo:    os.Getenv("BOOKER_EMAIL_TO"),
		selftest:   os.Getenv("BOOKER_SELFTEST") == "1",
		dryRun:     os.Getenv("BOOKER_DRY_RUN") == "1",
		oneshot:    os.Getenv("BOOKER_ONESHOT") == "1",
		targetDate: os.Getenv("BOOKER_TARGET_DATE"),
		retryFor:   30 * time.Second,
		logDir:     envOr("BOOKER_LOG_DIR", "."),
	}
	for _, id := range strings.Split(envOr("BOOKER_COURT_IDS", "372,373"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			c.courts = append(c.courts, id)
		}
	}
	if len(c.courts) == 0 {
		return nil, errors.New("BOOKER_COURT_IDS is empty")
	}
	if _, err := time.Parse("15:04", c.startTime); err != nil {
		return nil, fmt.Errorf("bad BOOKER_START_TIME %q", c.startTime)
	}
	if c.targetDate != "" {
		if _, err := time.Parse("2006-01-02", c.targetDate); err != nil {
			return nil, fmt.Errorf("bad BOOKER_TARGET_DATE %q", c.targetDate)
		}
	}
	if v := os.Getenv("BOOKER_FIRE_AT"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return nil, fmt.Errorf("bad BOOKER_FIRE_AT %q", v)
		}
		c.fireAt = t
	}
	if v := os.Getenv("BOOKER_RETRY_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("bad BOOKER_RETRY_WINDOW %q", v)
		}
		c.retryFor = d
	}
	if c.cookieRaw == "" && c.cookieFile == "" {
		return nil, errors.New("set BOOKER_COOKIE or BOOKER_COOKIE_FILE")
	}
	return c, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// timeRange turns "06:00" into "06:00 - 07:00".
func (c *config) timeRange() string {
	t, _ := time.Parse("15:04", c.startTime)
	return t.Format("15:04") + " - " + t.Add(time.Hour).Format("15:04")
}

func courtLabel(id string) string {
	if n, ok := courtNames[id]; ok {
		return id + " (" + n + ")"
	}
	return id
}

// ---- HTTP client with a simple cookie jar seeded from a DevTools header ----

type client struct {
	cfg    *config
	hc     *http.Client
	mu     sync.Mutex
	jar    map[string]string
	order  []string
	offset time.Duration // server clock minus local clock
}

func newClient(cfg *config) *client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 4
	tr.IdleConnTimeout = 5 * time.Minute
	return &client{
		cfg: cfg,
		hc: &http.Client{
			Transport:     tr,
			Timeout:       20 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func readCookieSource(cfg *config) (string, error) {
	raw := cfg.cookieRaw
	if raw == "" {
		b, err := os.ReadFile(cfg.cookieFile)
		if err != nil {
			return "", err
		}
		raw = string(b)
	}
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "cookie:")
	raw = strings.TrimPrefix(raw, "Cookie:")
	raw = strings.TrimSpace(raw)
	if !hasSessionCookie(raw) {
		return "", errors.New("cookie has no *_session entry (expected saraga_indonesia_v2_session or laravel_session)")
	}
	return raw, nil
}

func hasSessionCookie(raw string) bool {
	for _, part := range strings.Split(raw, ";") {
		name, _, _ := strings.Cut(strings.TrimSpace(part), "=")
		if strings.HasSuffix(name, "_session") {
			return true
		}
	}
	return false
}

func (c *client) loadCookies() error {
	raw, err := readCookieSource(c.cfg)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.jar = map[string]string{}
	c.order = nil
	for _, part := range strings.Split(raw, ";") {
		name, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || name == "" {
			continue
		}
		if _, seen := c.jar[name]; !seen {
			c.order = append(c.order, name)
		}
		c.jar[name] = val
	}
	return nil
}

func (c *client) cookieHeader() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	parts := make([]string, 0, len(c.order))
	for _, n := range c.order {
		if v, ok := c.jar[n]; ok {
			parts = append(parts, n+"="+v)
		}
	}
	return strings.Join(parts, "; ")
}

func (c *client) absorb(resp *http.Response) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ck := range resp.Cookies() {
		if ck.MaxAge < 0 {
			delete(c.jar, ck.Name)
			continue
		}
		if _, seen := c.jar[ck.Name]; !seen {
			c.order = append(c.order, ck.Name)
		}
		c.jar[ck.Name] = ck.Value
	}
}

type result struct {
	status   int
	location string
	body     string
}

func (c *client) do(method, path string, form url.Values) (*result, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, baseURL+path, body)
	if err != nil {
		return nil, err
	}
	c.prepare(req, form != nil)
	r, err := c.send(req)
	// GETs are safe to retry on transport errors (the egress path is flaky).
	for i := 0; err != nil && method == "GET" && i < 3; i++ {
		log.Printf("GET %s: %v (retry %d)", path, err, i+1)
		time.Sleep(time.Duration(200*(i+1)) * time.Millisecond)
		req, _ = http.NewRequest(method, baseURL+path, nil)
		c.prepare(req, false)
		r, err = c.send(req)
	}
	return r, err
}

func (c *client) prepare(req *http.Request, isForm bool) {
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9,id;q=0.8")
	req.Header.Set("Cookie", c.cookieHeader())
	if isForm {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", baseURL)
		req.Header.Set("Referer", baseURL+"/booking")
	}
}

func (c *client) send(req *http.Request) (*result, error) {
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	c.absorb(resp)
	if err != nil {
		return nil, err
	}
	return &result{status: resp.StatusCode, location: resp.Header.Get("Location"), body: string(b)}, nil
}

// getBooking loads /booking, returning the page and its CSRF token.
func (c *client) getBooking() (*result, string, error) {
	r, err := c.do("GET", "/booking", nil)
	if err != nil {
		return nil, "", err
	}
	if r.status >= 300 && r.status < 400 {
		if strings.Contains(r.location, "/login") {
			return r, "", errDead
		}
		return r, "", fmt.Errorf("/booking redirected to %s", r.location)
	}
	if r.status != 200 {
		return r, "", fmt.Errorf("/booking status %d", r.status)
	}
	m := csrfRe.FindStringSubmatch(r.body)
	if m == nil {
		return r, "", errors.New("no csrf-token meta on /booking")
	}
	return r, m[1], nil
}

// touch keeps the session alive, re-reading the cookie source once if it died.
func (c *client) touch() (string, error) {
	_, tok, err := c.getBooking()
	if errors.Is(err, errDead) {
		log.Printf("session dead, re-reading cookie source")
		if lerr := c.loadCookies(); lerr != nil {
			return "", fmt.Errorf("%w; reload failed: %v", err, lerr)
		}
		_, tok, err = c.getBooking()
	}
	return tok, err
}

// ---- server clock ----

func (c *client) serverNow() time.Time { return time.Now().Add(c.offset) }

// syncClock finds the local instant at which the server's Date header ticks
// over to a new second, giving an offset good to roughly half an RTT.
func (c *client) syncClock() error {
	type sample struct {
		mid    time.Time
		server time.Time
	}
	var prev *sample
	var offsets []time.Duration
	deadline := time.Now().Add(8 * time.Second)
	for len(offsets) < 3 && time.Now().Before(deadline) {
		req, _ := http.NewRequest("GET", baseURL+"/robots.txt", nil)
		req.Header.Set("User-Agent", ua)
		t0 := time.Now()
		resp, err := c.hc.Do(req)
		t1 := time.Now()
		if err != nil {
			return err
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		st, err := http.ParseTime(resp.Header.Get("Date"))
		if err != nil {
			return fmt.Errorf("bad Date header: %v", err)
		}
		s := &sample{mid: t0.Add(t1.Sub(t0) / 2), server: st}
		if prev != nil && s.server.After(prev.server) {
			boundary := prev.mid.Add(s.mid.Sub(prev.mid) / 2)
			offsets = append(offsets, s.server.Sub(boundary))
		}
		prev = s
	}
	if len(offsets) == 0 {
		return errors.New("could not observe a Date second boundary")
	}
	sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })
	c.offset = offsets[len(offsets)/2]
	return nil
}

// ---- schedule ----

func nextRelease(now time.Time) time.Time {
	n := now.In(wib)
	r := time.Date(n.Year(), n.Month(), n.Day(), 6, 0, 0, 0, wib)
	if !r.After(n) {
		r = r.AddDate(0, 0, 1)
	}
	return r
}

func (c *config) target(release time.Time) string {
	if c.targetDate != "" {
		return c.targetDate
	}
	return release.In(wib).AddDate(0, 0, 3).Format("2006-01-02")
}

func (c *config) courtList() string {
	s := make([]string, len(c.courts))
	for i, id := range c.courts {
		s[i] = courtLabel(id)
	}
	return strings.Join(s, ",")
}

// ---- booking ----

func (c *client) confirmationForm(token, court, date string) url.Values {
	return url.Values{
		"_token":     {token},
		"slug":       {slug},
		"court_id":   {court},
		"input_date": {date},
		"input_time": {fmt.Sprintf(`["%s"]`, c.cfg.timeRange())},
		"duration":   {"1"},
	}
}

// processForm starts from the hidden inputs of the rendered confirmation form
// and overlays the known fields.
func processForm(confirmBody, token string) url.Values {
	v := url.Values{}
	if m := formRe.FindStringSubmatch(confirmBody); m != nil {
		for _, in := range inputRe.FindAllStringSubmatch(m[1], -1) {
			attrs := map[string]string{}
			for _, a := range attrRe.FindAllStringSubmatch(in[1], -1) {
				attrs[strings.ToLower(a[1])] = html.UnescapeString(a[2])
			}
			if strings.EqualFold(attrs["type"], "hidden") && attrs["name"] != "" {
				v.Set(attrs["name"], attrs["value"])
			}
		}
	}
	if t := v.Get("_token"); t == "" {
		v.Set("_token", token)
	}
	v.Set("voucher_code", "")
	v.Set("total", "1")
	v.Set("grand_total", "1")
	v.Set("promo_discount", "0")
	v.Set("agreements", "confirm")
	return v
}

func (c *client) save(name, body string) string {
	p := filepath.Join(c.cfg.logDir, fmt.Sprintf("%s-%s.html", strings.ToLower(c.cfg.label), name))
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		log.Printf("save %s: %v", p, err)
	}
	return p
}

type outcome struct {
	ok     bool
	court  string
	detail string
}

func (c *client) attempt(release time.Time, token string, prebuilt map[string]*http.Request) outcome {
	date := c.cfg.target(release)
	deadline := release.Add(c.cfg.retryFor)
	var confirmBody, court string
	savedMiss := map[string]bool{}

	for round := 0; confirmBody == ""; round++ {
		for _, id := range c.cfg.courts {
			var req *http.Request
			if round == 0 && prebuilt[id] != nil {
				req = prebuilt[id]
			} else {
				f := c.confirmationForm(token, id, date)
				req, _ = http.NewRequest("POST", baseURL+"/booking/confirmation", strings.NewReader(f.Encode()))
				c.prepare(req, true)
			}
			sent := c.serverNow()
			r, err := c.send(req)
			if err != nil {
				log.Printf("confirmation court=%s err=%v", courtLabel(id), err)
				continue
			}
			log.Printf("confirmation court=%s sent=%s status=%d location=%q kode=%v",
				courtLabel(id), sent.In(wib).Format("15:04:05.000"), r.status, r.location, strings.Contains(r.body, "Kode Booking"))
			if r.status == 200 && strings.Contains(r.body, "Kode Booking") {
				confirmBody, court = r.body, id
				c.save("confirmation", r.body)
				break
			}
			if r.status == 419 {
				if _, t, err := c.getBooking(); err == nil {
					token = t
				}
			}
			if !savedMiss[id] {
				savedMiss[id] = true
				c.save("confirmation-miss-"+id, fmt.Sprintf("<!-- status=%d location=%s -->\n%s", r.status, r.location, r.body))
			}
		}
		if confirmBody != "" {
			break
		}
		if c.serverNow().After(deadline) {
			return outcome{detail: fmt.Sprintf("no court confirmed for %s %s after %d rounds", date, c.cfg.timeRange(), round+1)}
		}
		time.Sleep(250 * time.Millisecond)
	}

	pf := processForm(confirmBody, token)
	log.Printf("process_booking court=%s fields=%v", courtLabel(court), keys(pf))
	r, err := c.do("POST", "/booking/process_booking", pf)
	if err != nil {
		// Never retry blindly: the booking may have committed.
		log.Printf("process_booking err=%v (not retrying)", err)
	} else {
		log.Printf("process_booking status=%d location=%q", r.status, r.location)
		c.save("process", fmt.Sprintf("<!-- status=%d location=%s -->\n%s", r.status, r.location, r.body))
	}

	vr, _, verr := c.getBooking()
	if verr != nil && vr == nil {
		return outcome{court: court, detail: "process sent; verify GET /booking failed: " + verr.Error()}
	}
	p := c.save("verify", vr.body)
	found := verifyBody(vr.body, date, c.cfg.startTime)
	log.Printf("verify /booking saved=%s match=%v", p, found)
	if found {
		return outcome{ok: true, court: court, detail: fmt.Sprintf("booked %s %s %s (verified on /booking)", courtLabel(court), date, c.cfg.timeRange())}
	}
	return outcome{ok: err == nil && r.status < 400, court: court,
		detail: fmt.Sprintf("process_booking sent for %s %s %s; could not auto-verify, check %s", courtLabel(court), date, c.cfg.timeRange(), p)}
}

// verifyBody looks for the target date (in a few formats) and start time.
func verifyBody(body, date, start string) bool {
	d, _ := time.Parse("2006-01-02", date)
	forms := []string{date, d.Format("02-01-2006"), d.Format("02/01/2006"), d.Format("2 January 2006"), d.Format("02 Jan 2006"), d.Format("2 Jan 2006")}
	for _, f := range forms {
		if i := strings.Index(body, f); i >= 0 {
			lo, hi := max(0, i-2000), min(len(body), i+2000)
			if strings.Contains(body[lo:hi], start) {
				return true
			}
		}
	}
	return false
}

func keys(v url.Values) []string {
	k := make([]string, 0, len(v))
	for n := range v {
		k = append(k, n)
	}
	sort.Strings(k)
	return k
}

// ---- notifications ----

func (c *client) notify(title, msg string) {
	log.Printf("NOTIFY %s: %s", title, msg)
	if runtime.GOOS == "darwin" {
		script := fmt.Sprintf("display notification %q with title %q sound name \"Glass\"", msg, "booker "+c.cfg.label+": "+title)
		_ = exec.Command("osascript", "-e", script).Run()
	}
	if c.cfg.smtpUser != "" && c.cfg.smtpPass != "" && c.cfg.emailTo != "" {
		body := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: booker %s: %s\r\n\r\n%s\r\n", c.cfg.smtpUser, c.cfg.emailTo, c.cfg.label, title, msg)
		auth := smtp.PlainAuth("", c.cfg.smtpUser, c.cfg.smtpPass, "smtp.gmail.com")
		if err := smtp.SendMail("smtp.gmail.com:587", auth, c.cfg.smtpUser, []string{c.cfg.emailTo}, []byte(body)); err != nil {
			log.Printf("email failed: %v", err)
		}
	}
}

// ---- main ----

func sleepUntil(c *client, t time.Time) {
	for {
		d := t.Sub(c.serverNow())
		if d <= 0 {
			return
		}
		if d > 50*time.Millisecond {
			time.Sleep(d - 30*time.Millisecond)
			continue
		}
		for c.serverNow().Before(t) {
		}
		return
	}
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	log.SetPrefix("[" + cfg.label + "] ")
	c := newClient(cfg)
	if err := c.loadCookies(); err != nil {
		log.Fatalf("cookies: %v", err)
	}
	if err := c.syncClock(); err != nil {
		log.Printf("clock sync failed, using local clock: %v", err)
	}

	if cfg.selftest {
		os.Exit(selftest(c))
	}
	os.Exit(run(c))
}

func selftest(c *client) int {
	cfg := c.cfg
	release := nextRelease(c.serverNow())
	if !cfg.fireAt.IsZero() {
		release = cfg.fireAt
	}
	date, _ := time.Parse("2006-01-02", cfg.target(release))
	_, tok, err := c.getBooking()
	status := "alive"
	if err != nil {
		status = err.Error()
	}
	log.Printf("SELFTEST account=%s session=%s csrf=%v clock_offset=%s", cfg.label, status, tok != "", c.offset.Round(time.Millisecond))
	log.Printf("SELFTEST next_release=%s target=%s %s courts=%s dry_run=%v",
		release.In(wib).Format("Mon 2006-01-02 15:04:05 MST"), date.Format("Mon 2006-01-02"), cfg.timeRange(), cfg.courtList(), cfg.dryRun)
	if err != nil || tok == "" {
		log.Printf("SELFTEST FAIL")
		return 1
	}
	log.Printf("SELFTEST OK")
	return 0
}

func run(c *client) int {
	cfg := c.cfg
	for {
		release := nextRelease(c.serverNow())
		if !cfg.fireAt.IsZero() {
			release = cfg.fireAt
		}
		log.Printf("armed: target=%s %s courts=%s fire_at=%s clock_offset=%s dry_run=%v",
			cfg.target(release), cfg.timeRange(), cfg.courtList(),
			release.In(wib).Format("Mon 2006-01-02 15:04:05.000 MST"), c.offset.Round(time.Millisecond), cfg.dryRun)

		// Phase 1: touch every 10 minutes until T-3m.
		deadNotified := false
		for c.serverNow().Before(release.Add(-3 * time.Minute)) {
			if _, err := c.touch(); err != nil {
				log.Printf("touch: %v", err)
				if errors.Is(err, errDead) && !deadNotified {
					c.notify("session dead", "Refresh the cookie before "+release.In(wib).Format("Mon 15:04 MST"))
					deadNotified = true
				}
			} else {
				log.Printf("touch ok")
				deadNotified = false
			}
			wait := 10 * time.Minute
			if left := release.Add(-3 * time.Minute).Sub(c.serverNow()); left < wait {
				wait = left
			}
			if wait > 0 {
				time.Sleep(wait)
			}
		}

		// Phase 2: resync clock, keep the connection hot from T-2m.
		if err := c.syncClock(); err != nil {
			log.Printf("clock resync failed: %v", err)
		}
		log.Printf("clock offset %s", c.offset.Round(time.Millisecond))
		for c.serverNow().Before(release.Add(-15 * time.Second)) {
			if c.serverNow().After(release.Add(-2 * time.Minute)) {
				if _, err := c.touch(); err != nil {
					log.Printf("hot touch: %v", err)
				}
			}
			d := min(15*time.Second, release.Add(-15*time.Second).Sub(c.serverNow()))
			if d > 0 {
				time.Sleep(d)
			}
		}

		// Phase 3: fresh token and prebuilt requests at T-15s.
		token, err := c.touch()
		if err != nil {
			c.notify("FAILED", "session not usable at T-15s: "+err.Error())
			if cfg.oneshot || !cfg.fireAt.IsZero() {
				return 1
			}
			time.Sleep(time.Minute)
			continue
		}
		date := cfg.target(release)
		prebuilt := map[string]*http.Request{}
		for _, id := range cfg.courts {
			f := c.confirmationForm(token, id, date)
			req, _ := http.NewRequest("POST", baseURL+"/booking/confirmation", strings.NewReader(f.Encode()))
			c.prepare(req, true)
			prebuilt[id] = req
		}
		log.Printf("prebuilt %d requests for %s %s; firing at %s", len(prebuilt), date, cfg.timeRange(), release.In(wib).Format("15:04:05.000"))
		// One last warm-up so an idle connection is in the pool.
		sleepUntil(c, release.Add(-3*time.Second))
		c.do("GET", "/robots.txt", nil)

		sleepUntil(c, release)
		log.Printf("FIRE server=%s", c.serverNow().In(wib).Format("15:04:05.000"))
		var out outcome
		if cfg.dryRun {
			out = outcome{ok: true, detail: fmt.Sprintf("dry run: would book %s %s on %s", date, cfg.timeRange(), cfg.courtList())}
		} else {
			out = c.attempt(release, token, prebuilt)
		}
		if out.ok {
			c.notify("RESULT ok", out.detail)
		} else {
			c.notify("RESULT FAILED", out.detail)
		}
		if cfg.oneshot || !cfg.fireAt.IsZero() {
			if out.ok {
				return 0
			}
			return 1
		}
		time.Sleep(time.Minute)
	}
}
