package main

import (
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"
)

const oauthMaxCookies = 128

type oauthPendingCookie struct {
	URL    string      `json:"url"` // fixed upstream URL with query removed
	Cookie http.Cookie `json:"cookie"`
}

// cookiejar.Cookies intentionally omits path/domain/expiry. Recording SetCookies
// preserves those attributes, including host-only cookies and session cookies,
// instead of accidentally extending cookies or broadening their scope on restart.
type oauthCookieJar struct {
	mu       sync.Mutex
	jar      *cookiejar.Jar
	records  map[string]oauthPendingCookie
	overflow bool
}

func newOAuthCookieJar() (*oauthCookieJar, error) {
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return nil, err
	}
	return &oauthCookieJar{jar: jar, records: make(map[string]oauthPendingCookie)}, nil
}

func (j *oauthCookieJar) Cookies(u *url.URL) []*http.Cookie { return j.jar.Cookies(u) }

func (j *oauthCookieJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.jar.SetCookies(u, cookies)
	for _, original := range cookies {
		c := *original
		c.Raw, c.RawExpires, c.Unparsed = "", "", nil
		if c.Path == "" || c.Path[0] != '/' {
			c.Path = "/"
			if i := strings.LastIndex(u.Path, "/"); i > 0 {
				c.Path = u.Path[:i]
			}
		}
		domain := strings.TrimPrefix(strings.ToLower(c.Domain), ".")
		host := strings.ToLower(u.Hostname())
		if domain == "" {
			domain = host
		}
		// The real jar applies public-suffix and syntax checks too. Keeping rejected
		// entries would not grant extra cookies but would waste the bounded budget.
		if c.Valid() != nil || (domain != host && !strings.HasSuffix(host, "."+domain)) {
			continue
		}
		key := host + "\x00" + domain + "\x00" + c.Path + "\x00" + c.Name
		if c.MaxAge < 0 || (!c.Expires.IsZero() && !time.Now().Before(c.Expires) && c.MaxAge == 0) {
			delete(j.records, key)
			continue
		}
		if c.MaxAge > 0 {
			c.Expires = time.Now().Add(time.Duration(c.MaxAge) * time.Second)
			c.MaxAge = 0
		}
		clean := *u
		clean.RawQuery, clean.Fragment, clean.RawFragment, clean.User = "", "", "", nil
		if len(j.records) >= oauthMaxCookies {
			if _, exists := j.records[key]; !exists {
				j.overflow = true
				continue
			}
		}
		j.records[key] = oauthPendingCookie{URL: clean.String(), Cookie: c}
	}
}

func (j *oauthCookieJar) snapshot() []oauthPendingCookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.overflow {
		return make([]oauthPendingCookie, oauthMaxCookies+1)
	}
	out := make([]oauthPendingCookie, 0, len(j.records))
	for key, r := range j.records {
		if !r.Cookie.Expires.IsZero() && !time.Now().Before(r.Cookie.Expires) {
			delete(j.records, key)
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, k int) bool {
		return out[i].URL+out[i].Cookie.Name+out[i].Cookie.Path < out[k].URL+out[k].Cookie.Name+out[k].Cookie.Path
	})
	return out
}

func validOAuthPendingCookies(cookies []oauthPendingCookie, profile oauthRequestProfile) bool {
	if len(cookies) > oauthMaxCookies {
		return false
	}
	for _, r := range cookies {
		// Profile URL injection cannot become a request or cookie source. Cookies
		// only originate at one of the three fixed endpoints of the chosen realm.
		validURL := false
		for _, path := range []string{"/v2/plugin/auth/state", "/v2/plugin/auth/token", "/v2/plugin/login/account"} {
			if r.URL == profile.base+path {
				validURL = true
			}
		}
		if !validURL || r.Cookie.Valid() != nil || r.Cookie.MaxAge != 0 {
			return false
		}
	}
	return true
}

func restoreOAuthCookies(cookies []oauthPendingCookie) (*oauthCookieJar, error) {
	jar, err := newOAuthCookieJar()
	if err != nil {
		return nil, err
	}
	for _, r := range cookies {
		u, err := url.Parse(r.URL)
		if err != nil {
			return nil, errors.New("invalid login cookie snapshot")
		}
		cookie := r.Cookie
		jar.SetCookies(u, []*http.Cookie{&cookie})
	}
	return jar, nil
}
