package handlers

import (
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/jmoiron/sqlx"
	"zgo.at/goatcounter"
	"zgo.at/zhttp"
	"zgo.at/zlog"

	"github.com/mssola/user_agent"
	"zgo.at/goatcounter/cfg"
)

type Backend struct{}

func (h Backend) Mount(r chi.Router, db *sqlx.DB) {
	r.Use(
		middleware.RealIP,
		zhttp.Unpanic(cfg.Prod),
		addctx(db, true),
		zhttp.Headers(http.Header{
			"Strict-Transport-Security": []string{"max-age=2592000"},
			"X-Frame-Options":           []string{"SAMEORIGIN"},
			"X-Content-Type-Options":    []string{"nosniff"},
			// unsafe-inline on style is needed because we set style="height: .."
			// on the charts.
			"Content-Security-Policy": []string{fmt.Sprintf(
				"default-src %s; connect-src 'self'; style-src %[1]s 'unsafe-inline'",
				cfg.DomainStatic)},
		}),
		zhttp.Log(true, ""))

	// Don't allow any indexing of the backend interface by search engines.
	r.Get("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Cache-Control", "public, max-age=31536000")
		w.WriteHeader(200)
		w.Write([]byte("User-agent: *\nDisallow: /\n"))
	})

	// Counter that the script on the website calls.
	r.Get("/count", zhttp.Wrap(h.count))

	// Backend interface.
	a := r.With(keyAuth)
	a.Get("/", zhttp.Wrap(h.index))
	a.Get("/refs", zhttp.Wrap(h.refs))
	a.Get("/settings", zhttp.Wrap(h.settings))
	a.Post("/save", zhttp.Wrap(h.save))

	user{}.mount(a)
}

func (h Backend) count(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Cache-Control", "no-store,no-cache")

	// Don't track pages fetched with the browser's prefetch algorithm.
	// See https://github.com/usefathom/fathom/issues/13
	if r.Header.Get("X-Moz") == "prefetch" || r.Header.Get("X-Purpose") == "preview" {
		return zhttp.String(w, "")
	}
	if user_agent.New(r.UserAgent()).Bot() {
		return zhttp.String(w, "")
	}

	var hit goatcounter.Hit
	_, err := zhttp.Decode(r, &hit)
	if err != nil {
		return err
	}
	hit.Site = goatcounter.MustGetSite(r.Context()).ID
	hit.CreatedAt = time.Now().UTC()

	browser := goatcounter.BrowserStat{
		Site:      hit.Site,
		Browser:   r.UserAgent(),
		CreatedAt: hit.CreatedAt,
	}

	goatcounter.Memstore.Append(hit, browser)

	return zhttp.String(w, "")
}

const day = 24 * time.Hour

func (h Backend) index(w http.ResponseWriter, r *http.Request) error {
	// TODO(v1): cache much more aggressively for public displays. Don't care so
	// much if it's outdated by an hour.
	//
	// TODO(v1): also rate limit more for public.
	//
	// TODO(v1): Use period first as fallback when there's no JS.
	// p := r.URL.Query().Get("period")

	start := time.Now().Add(-7 * day)
	if s := r.URL.Query().Get("period-start"); s != "" {
		var err error
		start, err = time.Parse("2006-01-02", s)
		if err != nil {
			zhttp.FlashError(w, "start date: %s", err.Error())
			start = time.Now().Add(-7 * day)
		}
	}
	end := time.Now()
	if s := r.URL.Query().Get("period-end"); s != "" {
		var err error
		end, err = time.Parse("2006-01-02", s)
		if err != nil {
			zhttp.FlashError(w, "end date: %s", err.Error())
			end = time.Now()
		}
	}

	l := zlog.Debug("backend").Module("backend")

	var pages goatcounter.HitStats
	// TODO: for caching, we only need to fetch the last day, and then just
	// fetch the HTML for the older pages.
	// We can generate the HTML in the cron job.
	err, total := pages.List(r.Context(), start, end)
	if err != nil {
		return err
	}

	var browsers goatcounter.BrowserStats
	err = browsers.List(r.Context(), start, end)
	if err != nil {
		return err
	}

	// Add refers.
	sr := r.URL.Query().Get("showrefs")
	var refs goatcounter.HitStats
	if sr != "" {
		err := refs.ListRefs(r.Context(), sr, start, end)
		if err != nil {
			return err
		}
	}

	l = l.Since("fetch data")
	x := zhttp.Template(w, "backend.gohtml", struct {
		Globals
		ShowRefs    string
		PeriodStart time.Time
		PeriodEnd   time.Time
		Pages       goatcounter.HitStats
		Refs        goatcounter.HitStats
		TotalHits   int
		Browsers    goatcounter.BrowserStats
	}{newGlobals(w, r), sr, start, end, pages, refs, total, browsers})
	l = l.Since("exec template")
	return x
}

func (h Backend) refs(w http.ResponseWriter, r *http.Request) error {
	start, err := time.Parse("2006-01-02", r.URL.Query().Get("period-start"))
	if err != nil {
		return err
	}

	end, err := time.Parse("2006-01-02", r.URL.Query().Get("period-end"))
	if err != nil {
		return err
	}

	var refs goatcounter.HitStats
	err = refs.ListRefs(r.Context(), r.URL.Query().Get("showrefs"), start, end)
	if err != nil {
		return err
	}

	return zhttp.Template(w, "_backend_refs.gohtml", refs)
}

func (h Backend) settings(w http.ResponseWriter, r *http.Request) error {
	return zhttp.Template(w, "backend_settings.gohtml", struct {
		Globals
	}{newGlobals(w, r)})
}

func (h Backend) save(w http.ResponseWriter, r *http.Request) error {
	args := struct {
		Domain   string                   `json:"domain"`
		Settings goatcounter.SiteSettings `json:"settings"`
	}{}
	_, err := zhttp.Decode(r, &args)
	if err != nil {
		return err
	}

	site := goatcounter.MustGetSite(r.Context())
	site.Domain = args.Domain
	site.Settings = args.Settings

	err = site.Update(r.Context())
	if err != nil {
		zhttp.FlashError(w, "%v", err)
	} else {
		zhttp.Flash(w, "Saved!")
	}

	return zhttp.SeeOther(w, "/settings")
}
