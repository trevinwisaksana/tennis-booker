package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSite mimics the booking flow: court 372 is taken, 373 is free.
type fakeSite struct {
	mu        sync.Mutex
	opensAt   time.Time
	confirms  []string
	processed []string
	booked    string
}

func (f *fakeSite) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	loggedIn := func(r *http.Request) bool {
		ck, err := r.Cookie("saraga_indonesia_v2_session")
		return err == nil && ck.Value == "good"
	}
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			fmt.Fprint(w, `<meta name="csrf-token" content="logintok">`)
			return
		}
		r.ParseForm()
		if r.Form.Get("_token") == "logintok" && r.Form.Get("email") == "a@example.com" && r.Form.Get("password") == "pw" {
			http.SetCookie(w, &http.Cookie{Name: "saraga_indonesia_v2_session", Value: "good", Path: "/"})
			http.Redirect(w, r, "/home", http.StatusFound)
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	})
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
	mux.HandleFunc("/booking", func(w http.ResponseWriter, r *http.Request) {
		if !loggedIn(r) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		fmt.Fprintf(w, `<html><head><meta name="csrf-token" content="tok123"></head><body>%s</body></html>`, f.booked)
	})
	mux.HandleFunc("/booking/confirmation", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		f.confirms = append(f.confirms, r.Form.Get("court_id"))
		if r.Form.Get("_token") != "tok123" || r.Form.Get("slug") != slug || r.Form.Get("duration") != "1" {
			t.Errorf("bad confirmation form: %v", r.Form)
		}
		if time.Now().Before(f.opensAt) || r.Form.Get("court_id") == "372" {
			http.Redirect(w, r, "/booking", http.StatusFound)
			return
		}
		fmt.Fprintf(w, `<h1>Kode Booking</h1><form method="post" action="%s/booking/process_booking">
			<input type="hidden" name="_token" value="tok123">
			<input type="hidden" name="booking_ref" value="abc&amp;1">
			<input type="text" name="voucher_code" value="">
			<input type="checkbox" name="agreements" value="confirm"></form>`, "")
	})
	mux.HandleFunc("/booking/process_booking", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		f.processed = append(f.processed, r.Form.Encode())
		if r.Form.Get("booking_ref") != "abc&1" || r.Form.Get("agreements") != "confirm" || r.Form.Get("grand_total") != "1" {
			t.Errorf("bad process form: %v", r.Form)
		}
		f.booked = "Indoor 3 30 September 2026 06:00 - 07:00"
		http.Redirect(w, r, "/booking", http.StatusFound)
	})
	return mux
}

func TestEndToEnd(t *testing.T) {
	site := &fakeSite{}
	srv := httptest.NewServer(site.handler(t))
	defer srv.Close()
	baseURL = srv.URL

	fire := time.Now().Add(2 * time.Second).Truncate(time.Second).Add(time.Second)
	site.opensAt = fire.Add(400 * time.Millisecond) // server opens slightly late: retries must cover it
	cfg := &config{
		label: "T", cookieRaw: "XSRF-TOKEN=x; saraga_indonesia_v2_session=good",
		startTime: "06:00", courts: []string{"372", "373"},
		targetDate: "2026-09-30", fireAt: fire, oneshot: true,
		retryFor: 5 * time.Second, logDir: t.TempDir(),
	}
	c := newClient(cfg)
	if err := c.loadCookies(); err != nil {
		t.Fatal(err)
	}
	if code := run(c); code != 0 {
		t.Fatalf("run exit %d", code)
	}
	if len(site.processed) != 1 {
		t.Fatalf("process_booking called %d times, want exactly 1", len(site.processed))
	}
	if site.confirms[0] != "372" || site.confirms[len(site.confirms)-1] != "373" {
		t.Fatalf("court order wrong: %v", site.confirms)
	}
}

func TestDeadSession(t *testing.T) {
	site := &fakeSite{}
	srv := httptest.NewServer(site.handler(t))
	defer srv.Close()
	baseURL = srv.URL
	cfg := &config{label: "T", cookieRaw: "saraga_indonesia_v2_session=bad", startTime: "07:00", courts: []string{"372"}, logDir: t.TempDir()}
	c := newClient(cfg)
	c.loadCookies()
	if _, err := c.touch(); err == nil || !strings.Contains(err.Error(), "session dead") {
		t.Fatalf("want dead session, got %v", err)
	}
	if selftest(c) != 1 {
		t.Fatal("selftest should fail on dead session")
	}
}

func TestSchedule(t *testing.T) {
	now := time.Date(2026, 9, 26, 14, 39, 0, 0, wib)
	r := nextRelease(now)
	if want := time.Date(2026, 9, 27, 6, 0, 0, 0, wib); !r.Equal(want) {
		t.Fatalf("release %v want %v", r, want)
	}
	cfg := &config{startTime: "07:00"}
	if d := cfg.target(r); d != "2026-09-30" {
		t.Fatalf("target %s", d)
	}
	if cfg.timeRange() != "07:00 - 08:00" {
		t.Fatalf("range %q", cfg.timeRange())
	}
	if f := (&client{cfg: cfg}).confirmationForm("t", "372", "2026-09-30"); f.Get("input_time") != `["07:00 - 08:00"]` {
		t.Fatalf("input_time %q", f.Get("input_time"))
	}
	early := time.Date(2026, 9, 27, 5, 59, 59, 0, wib)
	if r := nextRelease(early); r.Day() != 27 {
		t.Fatalf("pre-06:00 release should be same day, got %v", r)
	}
}

func TestCookieCheck(t *testing.T) {
	if !hasSessionCookie("a=1; saraga_indonesia_v2_session=x") || !hasSessionCookie("laravel_session=y") {
		t.Fatal("should accept session cookies")
	}
	if hasSessionCookie("XSRF-TOKEN=z") {
		t.Fatal("should reject cookie without session")
	}
}

func TestPasswordLogin(t *testing.T) {
	site := &fakeSite{}
	srv := httptest.NewServer(site.handler(t))
	defer srv.Close()
	baseURL = srv.URL

	// Dead cookie falls back to password login.
	cfg := &config{label: "T", cookieRaw: "saraga_indonesia_v2_session=stale", email: "a@example.com", password: "pw", startTime: "06:00", courts: []string{"372"}, logDir: t.TempDir()}
	c := newClient(cfg)
	if err := c.establish(); err != nil {
		t.Fatalf("establish: %v", err)
	}
	if selftest(c) != 0 {
		t.Fatal("selftest should pass after login")
	}

	bad := newClient(&config{label: "T", email: "a@example.com", password: "wrong", startTime: "06:00", courts: []string{"372"}})
	if err := bad.establish(); err == nil || !strings.Contains(err.Error(), "login rejected") {
		t.Fatalf("want login rejected, got %v", err)
	}
}
