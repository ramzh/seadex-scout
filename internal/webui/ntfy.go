package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Notification kinds, also the names SEADEX_SCOUT_NTFY_EVENTS takes.
const (
	EventUpgrades = "upgrades" // a new upgrade or newer version appeared
	EventProblems = "problems" // a lasting problem or stalled checks, and their end
	EventErrors   = "errors"   // a one-off error, at most once per message per errorRepeat
	EventReports  = "reports"  // a report started from the dashboard finished
	EventDigest   = "digest"   // once a day: searches, grabs and flag fixes since the last one
)

// AllEvents is every kind, the default.
var AllEvents = []string{EventUpgrades, EventProblems, EventErrors, EventReports, EventDigest}

// digestHour is the local hour the daily digest goes out in.
const digestHour = 9

const (
	// maxUpgradeMessages is how many upgrades one check sends one by one; the
	// rest go in one summary message.
	maxUpgradeMessages = 10
	// errorRepeat is how long the same error message stays quiet after it was
	// sent: a fault that repeats on every check would otherwise send hourly.
	errorRepeat = 6 * time.Hour
	// sweepEvery is how often stalled checks and cleared problems are looked for.
	sweepEvery = time.Minute
	// maxNtfyText bounds a message; ntfy turns a longer one into an attachment.
	maxNtfyText = 3500
)

// NtfyConfig configures notifications to an ntfy topic.
type NtfyConfig struct {
	// TopicURL is the topic's full address, such as https://ntfy.sh/my-topic.
	TopicURL string
	// Token is an ntfy access token; Username and Password are the
	// alternative. All may be empty for an open topic.
	Token, Username, Password string
	Events                    []string
	// Optional also sends upgrades where an alt is already held, and
	// Unmonitored ones for unmonitored series, seasons and movies.
	Optional, Unmonitored bool
	// DashboardURL, when set, adds an Open dashboard button.
	DashboardURL string
}

// NotifyStatus is what the dashboard shows about notifications. It never
// carries a credential.
type NotifyStatus struct {
	Enabled   bool      `json:"enabled"`
	Target    string    `json:"target,omitempty"`
	Events    []string  `json:"events,omitempty"`
	Sent      int       `json:"sent"`
	LastSent  time.Time `json:"last_sent,omitzero"`
	LastError string    `json:"last_error,omitempty"`
	ErrorAt   time.Time `json:"error_at,omitzero"`
}

// Event is something the Store saw that may become a notification.
type Event struct {
	Kind string // EventUpgrades, EventProblems, EventErrors or EventReports
	// Lines are the finding lines of new upgrades, or the one log line the
	// event is about.
	Lines []Line
	// Resolved marks the end of a problem; Key names it ("stall" or a condition).
	Resolved bool
	Key      string
	// Text is a ready message for events with no log line (a stall, a report).
	Text string
}

// Notifier sends Store events to ntfy.
type Notifier struct {
	cfg           NtfyConfig
	server, topic string
	events        map[string]bool
	store         *Store
	client        *http.Client
	log           *slog.Logger
	now           func() time.Time

	mu         sync.Mutex
	lastErrors map[string]time.Time

	// digestOf, when set, says what happened since a moment, for the daily
	// digest; ok is false when nothing did. lastDigest is the day it last ran.
	digestOf   func(since time.Time) (text string, ok bool)
	lastDigest time.Time
}

// SetDigest sets what the daily digest reports. Run may already be going.
func (n *Notifier) SetDigest(f func(since time.Time) (string, bool)) {
	n.mu.Lock()
	n.digestOf = f
	n.mu.Unlock()
}

// NewNotifier checks cfg and returns a Notifier reading store's events.
func NewNotifier(cfg NtfyConfig, store *Store, log *slog.Logger) (*Notifier, error) {
	u, err := url.Parse(strings.TrimSpace(cfg.TopicURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("the ntfy URL must be an http or https address of a topic, such as https://ntfy.sh/my-topic")
	}
	path := strings.Trim(u.Path, "/")
	i := strings.LastIndex(path, "/")
	topic := path[i+1:]
	if topic == "" {
		return nil, errors.New("the ntfy URL names no topic: add it to the end, such as https://ntfy.sh/my-topic")
	}
	server := u.Scheme + "://" + u.Host
	if i > 0 {
		server += "/" + path[:i]
	}
	events := map[string]bool{}
	for _, e := range cfg.Events {
		events[strings.ToLower(strings.TrimSpace(e))] = true
	}
	n := &Notifier{cfg: cfg, server: server, topic: topic, events: events, store: store, log: log,
		client: &http.Client{Timeout: 30 * time.Second}, now: time.Now, lastErrors: map[string]time.Time{}}
	store.setNotify(func(s *NotifyStatus) {
		s.Enabled = true
		s.Target = server + "/" + topic
		s.Events = cfg.Events
	})
	return n, nil
}

// Run sends events as the Store produces them, and looks for stalled checks
// and cleared problems every minute, until ctx ends.
func (n *Notifier) Run(ctx context.Context) {
	events := n.store.enableEvents()
	t := time.NewTicker(sweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-events:
			n.handle(ctx, ev)
		case <-t.C:
			for _, ev := range n.store.sweep(n.now()) {
				n.handle(ctx, ev)
			}
			n.digest(ctx)
		}
	}
}

// digest sends the daily digest once a day, in digestHour local time. A
// restart in that hour can send a second one, which is harmless.
func (n *Notifier) digest(ctx context.Context) {
	now := n.now()
	n.mu.Lock()
	digestOf := n.digestOf
	n.mu.Unlock()
	if !n.events[EventDigest] || digestOf == nil || now.Hour() != digestHour {
		return
	}
	if y, m, d := n.lastDigest.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return
	}
	n.lastDigest = now
	text, ok := digestOf(now.Add(-24 * time.Hour))
	if !ok {
		return
	}
	m := &ntfyMessage{Title: "SeaDex Scout · last 24 hours", Message: text, Tags: []string{"bar_chart"}, Priority: 2}
	if n.cfg.DashboardURL != "" {
		m.Click = n.cfg.DashboardURL
	}
	if err := n.send(ctx, m); err != nil {
		n.failed(err)
	}
}

// SendTest sends a test message, for the dashboard's button.
func (n *Notifier) SendTest(ctx context.Context) error {
	return n.send(ctx, &ntfyMessage{
		Title: "SeaDex Scout test", Message: "Notifications from SeaDex Scout reach this topic.",
		Tags: []string{"white_check_mark"}, Priority: 3,
	})
}

func (n *Notifier) handle(ctx context.Context, ev Event) {
	if !n.events[ev.Kind] {
		return
	}
	var msgs []*ntfyMessage
	switch ev.Kind {
	case EventUpgrades:
		msgs = n.upgradeMessages(ev.Lines)
	case EventProblems:
		msgs = []*ntfyMessage{problemMessage(&ev)}
	case EventErrors:
		if m := n.errorMessage(&ev); m != nil {
			msgs = []*ntfyMessage{m}
		}
	case EventReports:
		msgs = []*ntfyMessage{{Title: "SeaDex Scout report finished", Message: ev.Text, Tags: []string{"page_facing_up"}, Priority: 3}}
	}
	for _, m := range msgs {
		if n.cfg.DashboardURL != "" && m.Click == "" {
			m.Click = n.cfg.DashboardURL
		}
		if err := n.send(ctx, m); err != nil {
			n.log.Warn("could not send an ntfy notification", "error", err)
		}
	}
}

// upgradeMessages is one message per new upgrade the filters let through,
// and a summary past maxUpgradeMessages.
func (n *Notifier) upgradeMessages(lines []Line) []*ntfyMessage {
	unmonitored := n.store.unmonitoredTest()
	var keep []Line
	for i := range lines {
		l := &lines[i]
		if !n.cfg.Optional && l.Str("status") == "better_release" && l.Str("current_tier") == "alt" {
			continue
		}
		if !n.cfg.Unmonitored && unmonitored(l) {
			continue
		}
		keep = append(keep, *l)
	}
	var msgs []*ntfyMessage
	for i := range keep {
		if i == maxUpgradeMessages {
			msgs = append(msgs, &ntfyMessage{
				Title:    fmt.Sprintf("%d more SeaDex upgrades", len(keep)-maxUpgradeMessages),
				Message:  "Open the dashboard to see them all.",
				Tags:     []string{"arrow_up"},
				Priority: 3,
			})
			break
		}
		msgs = append(msgs, n.upgradeMessage(&keep[i]))
	}
	return msgs
}

func (n *Notifier) upgradeMessage(l *Line) *ntfyMessage {
	newer := l.Str("status") == "newer_revision"
	why := "Neither best nor alt"
	tag := "arrow_up"
	switch {
	case newer:
		why, tag = "Newer version", "new"
	case l.Str("current_tier") == "alt":
		why = "You have a SeaDex alt"
	}
	current, best := l.Str("current_group"), l.Str("recommended_group")
	if newer {
		current += " " + l.Str("current_revision")
		best += " " + l.Str("recommended_revision")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s → %s", why, orDash(current), orDash(best))
	if size := l.Int("recommended_bytes"); size > 0 {
		fmt.Fprintf(&b, "\nBest: %s%s", fmtBytes(size), changeText(l, ""))
	}
	if alt := l.Str("alt_group"); alt != "" {
		fmt.Fprintf(&b, "\nAlt: %s", alt)
		if size := l.Int("alt_recommended_bytes"); size > 0 {
			fmt.Fprintf(&b, " %s%s", fmtBytes(size), changeText(l, "alt_"))
		}
	}
	if tags := l.Str("seadex_tags"); tags != "" {
		fmt.Fprintf(&b, "\n%s", tags)
	}

	m := &ntfyMessage{Title: titleOf(l), Message: b.String(), Tags: []string{tag}, Priority: 3}
	if id := l.Int("al_id"); id > 0 {
		m.Click = "https://seadex.moe/" + strconv.FormatInt(id, 10) + "/"
	}
	for _, link := range []struct{ label, url string }{
		{"Nyaa", l.Str("nyaa_url")},
		{cmpOr(l.Str("public_tracker"), "Tracker"), l.Str("public_url")},
		{cmpOr(l.Str("ab_tracker"), "AnimeBytes"), l.Str("ab_url")},
	} {
		if link.url != "" && len(m.Actions) == 0 {
			m.Actions = append(m.Actions, ntfyAction{Action: "view", Label: link.label, URL: link.url})
		}
	}
	if u := l.Str("arr_url"); u != "" {
		label := "Sonarr"
		if l.Str("arr") == "radarr" {
			label = "Radarr"
		}
		m.Actions = append(m.Actions, ntfyAction{Action: "view", Label: label, URL: u})
	}
	if n.cfg.DashboardURL != "" {
		m.Actions = append(m.Actions, ntfyAction{Action: "view", Label: "Dashboard", URL: n.cfg.DashboardURL})
	}
	return m
}

// problemMessage is the start or end of a lasting problem or a stall.
func problemMessage(ev *Event) *ntfyMessage {
	name := problemName(ev.Key)
	if ev.Resolved {
		return &ntfyMessage{Title: "Resolved: " + name, Message: cmpOr(ev.Text, "It has cleared."), Tags: []string{"white_check_mark"}, Priority: 3}
	}
	text := ev.Text
	if len(ev.Lines) > 0 {
		l := &ev.Lines[0]
		text = l.Msg
		if e := l.Str("error"); e != "" {
			text += ": " + e
		}
		if arr := l.Str("arr"); arr != "" {
			text += " (" + arr + ")"
		}
	}
	return &ntfyMessage{Title: "SeaDex Scout: " + name, Message: text, Tags: []string{"warning"}, Priority: 4}
}

func problemName(key string) string {
	switch key {
	case "stall":
		return "checks have stopped"
	case "seadex-unreachable":
		return "SeaDex unreachable"
	case "seadex-catalogue-fetch-failing":
		return "SeaDex catalogue download failing"
	case "seadex-window-oversize":
		return "too many SeaDex changes for quick checks"
	case "anilist-lookups-failing":
		return "AniList lookups failing"
	case "mapping-refresh-rejected":
		return "anime ID map update refused"
	case "library-walk-shrunk":
		return "library came back much smaller"
	case "library-walk-partial":
		return "library read incomplete"
	default:
		return key
	}
}

// errorMessage is a one-off error, or nil while the same message is quiet.
func (n *Notifier) errorMessage(ev *Event) *ntfyMessage {
	if len(ev.Lines) == 0 {
		return nil
	}
	l := &ev.Lines[0]
	n.mu.Lock()
	last, seen := n.lastErrors[l.Msg]
	if seen && n.now().Sub(last) < errorRepeat {
		n.mu.Unlock()
		return nil
	}
	n.lastErrors[l.Msg] = n.now()
	n.mu.Unlock()
	text := l.Msg
	if e := l.Str("error"); e != "" {
		text += ": " + e
	}
	return &ntfyMessage{Title: "SeaDex Scout error", Message: text + "\nRepeats of this error are held back for 6 hours.", Tags: []string{"rotating_light"}, Priority: 4}
}

type ntfyAction struct {
	Action string `json:"action"`
	Label  string `json:"label"`
	URL    string `json:"url"`
}

type ntfyMessage struct {
	Topic    string       `json:"topic"`
	Title    string       `json:"title,omitempty"`
	Message  string       `json:"message"`
	Tags     []string     `json:"tags,omitempty"`
	Priority int          `json:"priority,omitempty"`
	Click    string       `json:"click,omitempty"`
	Actions  []ntfyAction `json:"actions,omitempty"`
}

// send publishes m as JSON to the server root, which keeps titles with any
// characters intact (HTTP headers would not).
func (n *Notifier) send(ctx context.Context, m *ntfyMessage) error {
	m.Topic = n.topic
	m.Title = clip(m.Title, 250)
	m.Message = clip(m.Message, maxNtfyText)
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.server+"/", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	switch {
	case n.cfg.Token != "":
		req.Header.Set("Authorization", "Bearer "+n.cfg.Token)
	case n.cfg.Username != "":
		req.SetBasicAuth(n.cfg.Username, n.cfg.Password)
	}
	res, err := n.client.Do(req)
	if err != nil {
		var uerr interface{ Unwrap() error }
		if errors.As(err, &uerr) && uerr.Unwrap() != nil {
			err = uerr.Unwrap() // drops the URL the client wraps the cause in
		}
		n.failed(err)
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		detail, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		err := fmt.Errorf("ntfy answered %d: %s", res.StatusCode, strings.TrimSpace(string(detail)))
		n.failed(err)
		return err
	}
	n.store.setNotify(func(s *NotifyStatus) {
		s.Sent++
		s.LastSent = n.now().UTC()
	})
	return nil
}

func (n *Notifier) failed(err error) {
	n.store.setNotify(func(s *NotifyStatus) {
		s.LastError = err.Error()
		s.ErrorAt = n.now().UTC()
	})
}

func titleOf(l *Line) string {
	title := cmpOr(l.Str("title"), "Unknown title")
	switch {
	case l.Str("episodes") != "":
		return title + " · " + l.Str("episodes")
	case l.Str("arr") == "radarr":
		return title
	case l.Attrs["season"] != nil && l.Int("season") == 0:
		return title + " · Specials"
	case l.Attrs["season"] != nil:
		return title + " · Season " + strconv.FormatInt(l.Int("season"), 10)
	default:
		return title
	}
}

func changeText(l *Line, prefix string) string {
	if c, ok := l.Attrs[prefix+"size_change_bytes"]; ok && c != nil {
		change := l.Int(prefix + "size_change_bytes")
		sign := "+"
		if change < 0 {
			sign, change = "−", -change
		}
		return " (" + sign + fmtBytes(change) + ")"
	}
	if disk := l.Int("library_bytes"); disk > 0 {
		change := l.Int(prefix+"recommended_bytes") - disk
		sign := "+"
		if change < 0 {
			sign, change = "−", -change
		}
		return " (≈ " + sign + fmtBytes(change) + ")"
	}
	return ""
}

func fmtBytes(b int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	v := float64(b)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if v >= 100 || i == 0 {
		return fmt.Sprintf("%.0f %s", v, units[i])
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

func orDash(s string) string { return cmpOr(strings.TrimSpace(s), "–") }

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// clip cuts s to at most n bytes without splitting a character.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
