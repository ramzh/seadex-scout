package webui

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// QbitConfig configures sending releases to qBittorrent.
type QbitConfig struct {
	// URL is the Web UI address, such as http://192.168.1.10:8080.
	URL                string
	Username, Password string
	// Categories maps "sonarr" and "radarr" to the category a release of each
	// is added under; an empty category adds it with none.
	Categories map[string]string
}

// QbitStatus is what the page needs to know: whether the buttons show, and
// the category each app's releases go to. It never carries a credential.
type QbitStatus struct {
	Enabled    bool              `json:"enabled"`
	Categories map[string]string `json:"categories,omitempty"`
}

// Qbit adds torrents through qBittorrent's Web API (v2), signing in when the
// session is missing or expired.
type Qbit struct {
	cfg    QbitConfig
	base   string // the Web UI address, with any base path
	origin string // scheme://host[:port], what qBittorrent's CSRF check compares
	client *http.Client

	mu sync.Mutex // serializes sign-in and add, so a retry sees one session
}

// NewQbit checks cfg and returns a client.
func NewQbit(cfg QbitConfig) (*Qbit, error) {
	u, err := url.Parse(strings.TrimSpace(cfg.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("the qBittorrent URL must be the http or https address of its Web UI, such as http://192.168.1.10:8080")
	}
	jar, _ := cookiejar.New(nil)
	return &Qbit{
		cfg:    cfg,
		base:   strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/"),
		origin: u.Scheme + "://" + u.Host,
		client: &http.Client{Timeout: 30 * time.Second, Jar: jar},
	}, nil
}

// Status is the page's view of the configuration.
func (q *Qbit) Status() QbitStatus {
	return QbitStatus{Enabled: true, Categories: q.cfg.Categories}
}

// nyaaView is the only release page the button accepts. The torrent qBittorrent
// fetches is built from its number, so the page can never make the container
// or qBittorrent fetch any other address.
var nyaaView = regexp.MustCompile(`^https://nyaa\.si/view/(\d{1,12})/?$`)

// NyaaTorrentURL turns a Nyaa release page into its .torrent download.
func NyaaTorrentURL(view string) (string, bool) {
	m := nyaaView.FindStringSubmatch(strings.TrimSpace(view))
	if m == nil {
		return "", false
	}
	return "https://nyaa.si/download/" + m[1] + ".torrent", true
}

// errQbitAuth means qBittorrent answered as if no session were signed in.
var errQbitAuth = errors.New("qBittorrent wants a sign-in")

// AddNyaa adds the release at a Nyaa page under arr's category.
func (q *Qbit) AddNyaa(ctx context.Context, view, arr string) (string, error) {
	torrent, ok := NyaaTorrentURL(view)
	if !ok {
		return "", errors.New("only nyaa.si release pages can be sent")
	}
	if arr != "sonarr" && arr != "radarr" {
		return "", errors.New("unknown app")
	}
	category := strings.TrimSpace(q.cfg.Categories[arr])

	q.mu.Lock()
	defer q.mu.Unlock()
	err := q.add(ctx, torrent, category)
	if errors.Is(err, errQbitAuth) {
		if err = q.signIn(ctx); err == nil {
			err = q.add(ctx, torrent, category)
		}
	}
	return category, err
}

// Check signs in when needed and returns qBittorrent's version, for the
// dashboard's Test connection button.
func (q *Qbit) Check(ctx context.Context) (string, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	version, err := q.version(ctx)
	if errors.Is(err, errQbitAuth) {
		if err = q.signIn(ctx); err == nil {
			version, err = q.version(ctx)
		}
	}
	return version, err
}

func (q *Qbit) version(ctx context.Context) (string, error) {
	status, body, err := q.do(ctx, http.MethodGet, "/api/v2/app/version", nil)
	switch {
	case err != nil:
		return "", err
	case status == http.StatusForbidden || status == http.StatusUnauthorized:
		return "", errQbitAuth
	case status != http.StatusOK:
		return "", answerError("the version check", status, body)
	}
	return body, nil
}

// signIn signs in, or explains why it could not.
func (q *Qbit) signIn(ctx context.Context) error {
	if q.cfg.Username == "" {
		return errors.New("qBittorrent wants a sign-in: set the qBittorrent user name and password, " +
			"or tick \"Bypass authentication for clients in whitelisted IP subnets\" in its Web UI settings and add your LAN")
	}
	return q.login(ctx)
}

func (q *Qbit) add(ctx context.Context, torrent, category string) error {
	if category != "" {
		// Ensure the category exists: qBittorrent answers 409 when it already
		// does, which is fine, and adding under a missing one fails.
		status, body, err := q.post(ctx, "/api/v2/torrents/createCategory", url.Values{"category": {category}})
		switch {
		case err != nil:
			return err
		case status == http.StatusForbidden || status == http.StatusUnauthorized:
			return errQbitAuth
		case status == http.StatusBadRequest:
			return fmt.Errorf("qBittorrent refused the category %q as a name", category)
		case status/100 != 2 && status != http.StatusConflict:
			return answerError("creating the category", status, body)
		}
	}
	form := url.Values{"urls": {torrent}}
	if category != "" {
		form.Set("category", category)
	}
	status, body, err := q.post(ctx, "/api/v2/torrents/add", form)
	switch {
	case err != nil:
		return err
	case status == http.StatusForbidden || status == http.StatusUnauthorized:
		return errQbitAuth
	case status == http.StatusUnsupportedMediaType || strings.HasPrefix(body, "Fails"):
		return errors.New("qBittorrent could not add the torrent; it may already be in the list")
	case status/100 != 2:
		return answerError("the add", status, body)
	}
	return nil
}

// login signs in. qBittorrent 4 answers 200 "Ok." or "Fails."; qBittorrent 5
// can answer 204 with no body, so success is anything 2xx that is not "Fails".
func (q *Qbit) login(ctx context.Context) error {
	status, body, err := q.post(ctx, "/api/v2/auth/login", url.Values{"username": {q.cfg.Username}, "password": {q.cfg.Password}})
	switch {
	case err != nil:
		return err
	case status == http.StatusForbidden:
		return errors.New("qBittorrent has banned this address after too many failed sign-ins; wait, or restart qBittorrent")
	case status == http.StatusUnauthorized:
		return errors.New("qBittorrent answered 401 to the sign-in, which is its security check rather than the password: " +
			"in its Web UI settings, untick \"Enable Host header validation\" or add this address to \"Server domains\", " +
			"or untick \"Enable Cross-Site Request Forgery (CSRF) protection\"" + bodyNote(body))
	case status/100 == 2 && strings.HasPrefix(body, "Fails"):
		return errors.New("qBittorrent refused the user name or password")
	case status/100 != 2:
		return answerError("the sign-in", status, body)
	}
	return nil
}

func answerError(what string, status int, body string) error {
	return fmt.Errorf("qBittorrent answered %d to %s%s", status, what, bodyNote(body))
}

func bodyNote(body string) string {
	if body == "" {
		return ""
	}
	return ` ("` + body + `")`
}

func (q *Qbit) post(ctx context.Context, path string, form url.Values) (int, string, error) {
	return q.do(ctx, http.MethodPost, path, form)
}

// do sends a request, a form when form is set, and returns the status and a
// short body. The Referer and Origin name qBittorrent itself, which its CSRF
// check requires.
func (q *Qbit) do(ctx context.Context, method, path string, form url.Values) (int, string, error) {
	return q.doLimit(ctx, method, path, form, 512)
}

// QbitTorrent is the part of a torrent the library views read.
type QbitTorrent struct {
	Hash        string  `json:"hash"`
	Name        string  `json:"name"`
	Category    string  `json:"category"`
	Comment     string  `json:"comment"`
	Tracker     string  `json:"tracker"`
	ContentPath string  `json:"content_path"`
	SavePath    string  `json:"save_path"`
	Private     bool    `json:"private"`
	Size        int64   `json:"size"`
	Progress    float64 `json:"progress"`
	AddedOn     int64   `json:"added_on"`
}

// QbitFile is one file of a torrent; Name is relative to the save path.
type QbitFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// readJSON GETs path and decodes the answer, signing in once when the
// session is missing or expired.
func (q *Qbit) readJSON(ctx context.Context, path string, out any) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for try := 0; ; try++ {
		status, body, err := q.doLimit(ctx, http.MethodGet, path, nil, 256<<20)
		switch {
		case err != nil:
			return err
		case (status == http.StatusForbidden || status == http.StatusUnauthorized) && try == 0:
			if err := q.signIn(ctx); err != nil {
				return err
			}
			continue
		case status != http.StatusOK:
			return answerError("the read of "+strings.SplitN(path, "?", 2)[0], status, firstBytes(body, 200))
		}
		return json.Unmarshal([]byte(body), out)
	}
}

func firstBytes(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// Torrents lists every torrent.
func (q *Qbit) Torrents(ctx context.Context) ([]QbitTorrent, error) {
	var out []QbitTorrent
	return out, q.readJSON(ctx, "/api/v2/torrents/info", &out)
}

// Files lists a torrent's files.
func (q *Qbit) Files(ctx context.Context, hash string) ([]QbitFile, error) {
	var out []QbitFile
	return out, q.readJSON(ctx, "/api/v2/torrents/files?hash="+url.QueryEscape(hash), &out)
}

// Delete removes a torrent, and its data when deleteFiles is set.
func (q *Qbit) Delete(ctx context.Context, hash string, deleteFiles bool) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	form := url.Values{"hashes": {hash}, "deleteFiles": {strconv.FormatBool(deleteFiles)}}
	for try := 0; ; try++ {
		status, body, err := q.post(ctx, "/api/v2/torrents/delete", form)
		switch {
		case err != nil:
			return err
		case (status == http.StatusForbidden || status == http.StatusUnauthorized) && try == 0:
			if err := q.signIn(ctx); err != nil {
				return err
			}
			continue
		case status/100 != 2:
			return answerError("the delete", status, body)
		}
		return nil
	}
}

// Park moves a torrent to its category's "-imported" twin, which has the same
// save path, so Sonarr or Radarr stop tracking it while it keeps seeding and
// no file moves (Auto TMM follows the category's path). The twin is created
// when missing; one with another save path is refused.
func (q *Qbit) Park(ctx context.Context, hash string) (string, error) {
	var torrents []QbitTorrent
	if err := q.readJSON(ctx, "/api/v2/torrents/info?hashes="+url.QueryEscape(hash), &torrents); err != nil {
		return "", err
	}
	if len(torrents) != 1 || torrents[0].Category == "" {
		return "", errors.New("the torrent or its category was not found in qBittorrent")
	}
	t := torrents[0]
	twin := t.Category + "-imported"
	var cats map[string]struct {
		SavePath string `json:"savePath"`
	}
	if err := q.readJSON(ctx, "/api/v2/torrents/categories", &cats); err != nil {
		return "", err
	}
	path := cats[t.Category].SavePath
	if c, ok := cats[twin]; ok && filepath.Clean(c.SavePath) != filepath.Clean(path) {
		return "", fmt.Errorf("category %s saves to %s, not %s: moving the torrent there would move its files", twin, c.SavePath, path)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := cats[twin]; !ok {
		if status, body, err := q.post(ctx, "/api/v2/torrents/createCategory", url.Values{"category": {twin}, "savePath": {path}}); err != nil || status/100 != 2 {
			return "", cmp.Or(err, answerError("creating the category", status, body))
		}
	}
	if status, body, err := q.post(ctx, "/api/v2/torrents/setCategory", url.Values{"hashes": {hash}, "category": {twin}}); err != nil || status/100 != 2 {
		return "", cmp.Or(err, answerError("the category change", status, body))
	}
	return twin, nil
}

func (q *Qbit) doLimit(ctx context.Context, method, path string, form url.Values, limit int64) (int, string, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, q.base+path, body)
	if err != nil {
		return 0, "", errors.New("the qBittorrent URL is not valid")
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Referer", q.base+"/")
	req.Header.Set("Origin", q.origin)
	res, err := q.client.Do(req)
	if err != nil {
		var uerr interface{ Unwrap() error }
		if errors.As(err, &uerr) && uerr.Unwrap() != nil {
			err = uerr.Unwrap() // drops the URL the client wraps the cause in
		}
		return 0, "", fmt.Errorf("could not reach qBittorrent: %w", err)
	}
	defer res.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(res.Body, limit))
	return res.StatusCode, strings.TrimSpace(string(answer)), nil
}
