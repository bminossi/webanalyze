package webanalyze

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/bobesa/go-domain-util/domainutil"
)

const VERSION = "1.0"

var (
	timeout = 8 * time.Second
	wa      *WebAnalyzer
)

// Result type encapsulates the result information from a given host
type Result struct {
	Host     string        `json:"host"`
	Matches  []Match       `json:"matches"`
	Duration time.Duration `json:"duration"`
	Error    error         `json:"error"`
}

// Match type encapsulates the App information from a match on a document
type Match struct {
	App     `json:"app"`
	AppName string     `json:"app_name"`
	Matches [][]string `json:"matches"`
	Version string     `json:"version"`
}

// WebAnalyzer types holds an analyzation job
type WebAnalyzer struct {
	appDefs *AppsDefinition
}

func (m *Match) updateVersion(version string) {
	if version != "" {
		m.Version = version
	}
}

// NewWebAnalyzer returns an analyzer struct for an ongoing job, which may be
func NewWebAnalyzer(appsFile string) (*WebAnalyzer, error) {
	var err error
	wa := new(WebAnalyzer)
	if err = wa.loadApps(appsFile); err != nil {
		return nil, err
	}
	return wa, nil
}

// worker loops until channel is closed. processes a single host at once
func (wa *WebAnalyzer) Process(job *Job) Result {

	// fix missing http scheme
	u, err := url.Parse(job.URL)
	if u.Scheme == "" {
		u.Scheme = "http"
	}
	job.URL = u.String()

	// measure time
	t0 := time.Now()
	result, err := process(job, wa.appDefs)
	t1 := time.Now()

	res := Result{
		Host:     job.URL,
		Matches:  result,
		Duration: t1.Sub(t0),
		Error:    err,
	}
	return res
}

func (wa *WebAnalyzer) CategoryById(cid string) string {
	return wa.appDefs.Cats[cid].Name
}

func fetchHost(host string) (*http.Response, error) {
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			Proxy:           http.ProxyFromEnvironment,
		}}

	req, err := http.NewRequest("GET", host, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Add("Accept", "*/*")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func unique(strSlice []string) []string {
	keys := make(map[string]bool)
	list := []string{}
	for _, entry := range strSlice {
		if _, value := keys[entry]; !value {
			keys[entry] = true
			list = append(list, entry)
		}
	}
	return list
}

func sameUrl(u1, u2 *url.URL) bool {
	return u1.Hostname() == u2.Hostname() &&
		u1.Port() == u2.Port() &&
		u1.RequestURI() == u2.RequestURI()
}

func parseLinks(doc *goquery.Document, base *url.URL, searchSubdomain bool) []string {
	var links []string

	doc.Find("a").Each(func(i int, s *goquery.Selection) {
		val, ok := s.Attr("href")
		if !ok {
			return
		}

		u, err := url.Parse(val)
		if err != nil {
			return
		}

		urlResolved := base.ResolveReference(u)

		if !searchSubdomain && urlResolved.Hostname() != base.Hostname() {
			return
		}

		if searchSubdomain && !isSubdomain(base, u) {
			return
		}

		if urlResolved.RequestURI() == "" {
			urlResolved.Path = "/"
		}

		if sameUrl(base, urlResolved) {
			return
		}

		links = append(links, urlResolved.String())

	})

	return unique(links)
}

func isSubdomain(base, u *url.URL) bool {
	return domainutil.Domain(base.String()) == domainutil.Domain(u.String())
}

// do http request and analyze response
func process(job *Job, appDefs *AppsDefinition) ([]Match, error) {
	var apps = make([]Match, 0)
	var err error

	var cookies []*http.Cookie
	var cookiesMap = make(map[string]string)
	var body []byte
	var headers http.Header

	// get response from host if allowed
	if job.forceNotDownload {
		body = job.Body
		headers = job.Headers
		cookies = job.Cookies
	} else {
		resp, err := fetchHost(job.URL)
		if err != nil {
			return nil, fmt.Errorf("Failed to retrieve")
		}

		defer resp.Body.Close()

		body, err = ioutil.ReadAll(resp.Body)
		if err == nil {
			headers = resp.Header
			cookies = resp.Cookies()
		}
	}

	for _, c := range cookies {
		cookiesMap[c.Name] = c.Value
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	// handle crawling
	if job.Crawl > 0 {
		base, _ := url.Parse(job.URL)

		for c, _ := range parseLinks(doc, base, job.SearchSubdomain) {
			if c >= job.Crawl {
				break
			}

			//newJob := NewOnlineJob(link, "", nil, 0, false)
		}
	}

	for appname, app := range appDefs.Apps {
		// TODO: Reduce complexity in this for-loop by functionalising out
		// the sub-loops and checks.

		findings := Match{
			App:     app,
			AppName: appname,
			Matches: make([][]string, 0),
		}

		// check raw html
		if m, v := findMatches(string(body), app.HTMLRegex); len(m) > 0 {
			findings.Matches = append(findings.Matches, m...)
			findings.updateVersion(v)
		}

		// check response header
		headerFindings, version := app.FindInHeaders(headers)
		findings.Matches = append(findings.Matches, headerFindings...)
		findings.updateVersion(version)

		// check url
		if m, v := findMatches(job.URL, app.URLRegex); len(m) > 0 {
			findings.Matches = append(findings.Matches, m...)
			findings.updateVersion(v)
		}

		// check script tags
		doc.Find("script").Each(func(i int, s *goquery.Selection) {
			if script, exists := s.Attr("src"); exists {
				if m, v := findMatches(script, app.ScriptRegex); len(m) > 0 {
					findings.Matches = append(findings.Matches, m...)
					findings.updateVersion(v)
				}
			}
		})

		// check meta tags
		for _, h := range app.MetaRegex {
			selector := fmt.Sprintf("meta[name='%s']", h.Name)
			doc.Find(selector).Each(func(i int, s *goquery.Selection) {
				content, _ := s.Attr("content")
				if m, v := findMatches(content, []AppRegexp{h}); len(m) > 0 {
					findings.Matches = append(findings.Matches, m...)
					findings.updateVersion(v)
				}
			})
		}

		// check cookies
		for _, c := range app.CookieRegex {
			if _, ok := cookiesMap[c.Name]; ok {

				// if there is a regexp set, ensure it matches.
				// otherwise just add this as a match
				if c.Regexp != nil {

					// only match single AppRegexp on this specific cookie
					if m, v := findMatches(cookiesMap[c.Name], []AppRegexp{c}); len(m) > 0 {
						findings.Matches = append(findings.Matches, m...)
						findings.updateVersion(v)
					}

				} else {
					findings.Matches = append(findings.Matches, []string{c.Name})
				}
			}

		}

		if len(findings.Matches) > 0 {
			apps = append(apps, findings)

			// handle implies
			for _, implies := range app.Implies {
				for implyAppname, implyApp := range appDefs.Apps {
					if implies != implyAppname {
						continue
					}

					f2 := Match{
						App:     implyApp,
						AppName: implyAppname,
						Matches: make([][]string, 0),
					}
					apps = append(apps, f2)
				}

			}
		}
	}

	return apps, nil
}

// runs a list of regexes on content
func findMatches(content string, regexes []AppRegexp) ([][]string, string) {
	var m [][]string
	var version string

	for _, r := range regexes {
		matches := r.Regexp.FindAllStringSubmatch(content, -1)
		if matches == nil {
			continue
		}

		m = append(m, matches...)

		if r.Version != "" {
			version = findVersion(m, r.Version)
		}

	}
	return m, version
}

// parses a version against matches
func findVersion(matches [][]string, version string) string {
	var v string

	for _, matchPair := range matches {
		// replace backtraces (max: 3)
		for i := 1; i <= 3; i++ {
			bt := fmt.Sprintf("\\%v", i)
			if strings.Contains(version, bt) && len(matchPair) >= i {
				v = strings.Replace(version, bt, matchPair[i], 1)
			}
		}

		// return first found version
		if v != "" {
			return v
		}

	}

	return ""
}
