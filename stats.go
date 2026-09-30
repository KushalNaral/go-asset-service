package main

import (
	"encoding/json"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Outcomes of a request, also sent to clients as X-Cache.
const (
	OutcomeHit         = "HIT"         // served from memory, fresh
	OutcomeRevalidated = "REVALIDATED" // expired, source said unchanged, served from memory
	OutcomeMiss        = "MISS"        // fetched from the source and cached
	OutcomeStale       = "STALE"       // expired and the source failed: served the old copy
	OutcomeBypass      = "BYPASS"      // too large to cache, streamed through
	OutcomeNotFound    = "NOT_FOUND"   // the source has no such file
	OutcomeError       = "ERROR"       // the source failed and nothing was cached
)

var latencyBoundsMs = []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500}

// RequestRecord is one line of the live request log.
type RequestRecord struct {
	Time       time.Time `json:"time"`
	Path       string    `json:"path"`
	Variant    string    `json:"variant,omitempty"`
	Outcome    string    `json:"outcome"`
	Status     int       `json:"status"`
	Bytes      int64     `json:"bytes"`
	DurationMs float64   `json:"duration_ms"`
	ClientIP   string    `json:"client_ip"`
	Referer    string    `json:"referer,omitempty"`
	UserAgent  string    `json:"user_agent,omitempty"`
	Device     string    `json:"device"`
	Browser    string    `json:"browser"`
}

type Totals struct {
	Requests        int64            `json:"requests"`
	BytesServed     int64            `json:"bytes_served"`
	Outcomes        map[string]int64 `json:"outcomes"`
	StatusCodes     map[string]int64 `json:"status_codes"`
	DurationMsTotal float64          `json:"duration_ms_total"`
	Latency         []int64          `json:"latency_buckets"` // counts per latencyBoundsMs, last = slower

	OriginFetches     int64 `json:"origin_fetches"`
	OriginNotModified int64 `json:"origin_not_modified"`
	OriginNotFound    int64 `json:"origin_not_found"`
	OriginErrors      int64 `json:"origin_errors"`
	OriginBytes       int64 `json:"origin_bytes"`
	OriginMsTotal     int64 `json:"origin_ms_total"`

	Transforms       int64 `json:"transforms"`
	TransformMsTotal int64 `json:"transform_ms_total"`
	TransformErrors  int64 `json:"transform_errors"`
}

// Bucket is one minute or one hour of traffic.
type Bucket struct {
	Start       int64 `json:"start"` // unix seconds
	Requests    int64 `json:"requests"`
	Hits        int64 `json:"hits"`   // HIT + REVALIDATED + STALE
	Misses      int64 `json:"misses"` // MISS + BYPASS
	NotFound    int64 `json:"not_found"`
	Errors      int64 `json:"errors"`
	Bytes       int64 `json:"bytes"`
	OriginBytes int64 `json:"origin_bytes"`
}

// CountItem is one row of a "top N" list.
type CountItem struct {
	Key      string    `json:"key"`
	Count    int64     `json:"count"`
	Bytes    int64     `json:"bytes"`
	LastSeen time.Time `json:"last_seen"`
}

// Counter keeps the most frequent keys, dropping the least used half when full.
type Counter struct {
	Items map[string]*CountItem `json:"items"`
	Cap   int                   `json:"cap"`
}

func newCounter(capacity int) *Counter {
	return &Counter{Items: map[string]*CountItem{}, Cap: capacity}
}

func (c *Counter) add(key string, bytes int64, now time.Time) {
	it, ok := c.Items[key]
	if !ok {
		if len(c.Items) >= c.Cap {
			c.prune()
		}
		it = &CountItem{Key: key}
		c.Items[key] = it
	}
	it.Count++
	it.Bytes += bytes
	it.LastSeen = now
}

func (c *Counter) prune() {
	items := c.sorted()
	for _, it := range items[len(items)/2:] {
		delete(c.Items, it.Key)
	}
}

func (c *Counter) sorted() []CountItem {
	out := make([]CountItem, 0, len(c.Items))
	for _, it := range c.Items {
		out = append(out, *it)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].LastSeen.After(out[j].LastSeen)
	})
	return out
}

func (c *Counter) Top(n int) []CountItem {
	s := c.sorted()
	if len(s) > n {
		s = s[:n]
	}
	return s
}

// PathStat is everything known about one file, cached or not.
type PathStat struct {
	Path        string           `json:"path"`
	Requests    int64            `json:"requests"`
	Hits        int64            `json:"hits"`
	Misses      int64            `json:"misses"`
	NotFound    int64            `json:"not_found"`
	Errors      int64            `json:"errors"`
	Bytes       int64            `json:"bytes"`
	FirstSeen   time.Time        `json:"first_seen"`
	LastSeen    time.Time        `json:"last_seen"`
	LastStatus  int              `json:"last_status"`
	LastOutcome string           `json:"last_outcome"`
	Referers    map[string]int64 `json:"referers"`
	Variants    map[string]int64 `json:"variants"`
}

const (
	maxPaths          = 20000
	maxRefsPerPath    = 20
	maxVariantsPerKey = 20
	recentCapacity    = 1000
	minuteBuckets     = 180     // 3 hours
	hourBuckets       = 24 * 14 // 2 weeks
)

type Stats struct {
	mu sync.Mutex

	Since   time.Time `json:"since"`
	Totals  Totals    `json:"totals"`
	Minutes []Bucket  `json:"minutes"`
	Hours   []Bucket  `json:"hours"`

	RefererHosts *Counter `json:"referer_hosts"`
	RefererPages *Counter `json:"referer_pages"`
	Clients      *Counter `json:"clients"`
	Devices      *Counter `json:"devices"`
	Browsers     *Counter `json:"browsers"`
	Bots         *Counter `json:"bots"`
	ContentTypes *Counter `json:"content_types"`
	Folders      *Counter `json:"folders"`

	Paths map[string]*PathStat `json:"paths"`

	Recent     []RequestRecord `json:"recent"` // ring buffer
	RecentNext int             `json:"recent_next"`

	LastOriginError   string     `json:"last_origin_error"`
	LastOriginErrorAt *time.Time `json:"last_origin_error_at"`
}

func NewStats(now time.Time) *Stats {
	s := &Stats{}
	s.reset(now)
	return s
}

func (s *Stats) reset(now time.Time) {
	s.Since = now
	s.Totals = Totals{Outcomes: map[string]int64{}, StatusCodes: map[string]int64{}, Latency: make([]int64, len(latencyBoundsMs)+1)}
	s.Minutes = nil
	s.Hours = nil
	s.RefererHosts = newCounter(500)
	s.RefererPages = newCounter(2000)
	s.Clients = newCounter(5000)
	s.Devices = newCounter(10)
	s.Browsers = newCounter(50)
	s.Bots = newCounter(100)
	s.ContentTypes = newCounter(50)
	s.Folders = newCounter(500)
	s.Paths = map[string]*PathStat{}
	s.Recent = make([]RequestRecord, 0, recentCapacity)
	s.RecentNext = 0
	s.LastOriginError = ""
	s.LastOriginErrorAt = nil
}

func (s *Stats) Reset(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reset(now)
}

// Record adds one finished request.
func (s *Stats) Record(r RequestRecord, contentType string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := r.Time
	t := &s.Totals
	t.Requests++
	t.BytesServed += r.Bytes
	t.Outcomes[r.Outcome]++
	t.StatusCodes[strconv.Itoa(r.Status)]++
	t.DurationMsTotal += r.DurationMs
	t.Latency[latencyBucket(r.DurationMs)]++

	for _, b := range []*Bucket{bucketFor(&s.Minutes, now, time.Minute, minuteBuckets), bucketFor(&s.Hours, now, time.Hour, hourBuckets)} {
		b.Requests++
		b.Bytes += r.Bytes
		switch r.Outcome {
		case OutcomeHit, OutcomeRevalidated, OutcomeStale:
			b.Hits++
		case OutcomeMiss, OutcomeBypass:
			b.Misses++
		case OutcomeNotFound:
			b.NotFound++
		case OutcomeError:
			b.Errors++
		}
	}

	refHost, refPage := refererParts(r.Referer)
	s.RefererHosts.add(refHost, r.Bytes, now)
	s.RefererPages.add(refPage, r.Bytes, now)
	s.Clients.add(r.ClientIP, r.Bytes, now)
	s.Devices.add(r.Device, r.Bytes, now)
	if r.Device == "bot" {
		s.Bots.add(r.Browser, r.Bytes, now)
	} else {
		s.Browsers.add(r.Browser, r.Bytes, now)
	}
	if contentType != "" {
		s.ContentTypes.add(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]), r.Bytes, now)
	}
	s.Folders.add(folderOf(r.Path), r.Bytes, now)

	p := s.Paths[r.Path]
	if p == nil {
		if len(s.Paths) >= maxPaths {
			s.prunePaths()
		}
		p = &PathStat{Path: r.Path, FirstSeen: now, Referers: map[string]int64{}, Variants: map[string]int64{}}
		s.Paths[r.Path] = p
	}
	p.Requests++
	p.Bytes += r.Bytes
	p.LastSeen = now
	p.LastStatus = r.Status
	p.LastOutcome = r.Outcome
	switch r.Outcome {
	case OutcomeHit, OutcomeRevalidated, OutcomeStale:
		p.Hits++
	case OutcomeMiss, OutcomeBypass:
		p.Misses++
	case OutcomeNotFound:
		p.NotFound++
	case OutcomeError:
		p.Errors++
	}
	addCapped(p.Referers, refPage, maxRefsPerPath)
	if r.Variant != "" {
		addCapped(p.Variants, r.Variant, maxVariantsPerKey)
	}

	if len(s.Recent) < recentCapacity {
		s.Recent = append(s.Recent, r)
	} else {
		s.Recent[s.RecentNext] = r
	}
	s.RecentNext = (s.RecentNext + 1) % recentCapacity
}

func (s *Stats) RecordOrigin(status int, bytes int64, ms int64, err error, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := &s.Totals
	t.OriginFetches++
	t.OriginMsTotal += ms
	switch {
	case err != nil:
		t.OriginErrors++
		msg := err.Error()
		s.LastOriginError = msg
		s.LastOriginErrorAt = &now
	case status == 304:
		t.OriginNotModified++
	case status == 404:
		t.OriginNotFound++
	}
	s.addOriginBytes(bytes, now)
}

// RecordOriginBytes counts bytes streamed from the source after the fetch was recorded.
func (s *Stats) RecordOriginBytes(bytes int64, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addOriginBytes(bytes, now)
}

func (s *Stats) addOriginBytes(bytes int64, now time.Time) {
	if bytes <= 0 {
		return
	}
	s.Totals.OriginBytes += bytes
	bucketFor(&s.Minutes, now, time.Minute, minuteBuckets).OriginBytes += bytes
	bucketFor(&s.Hours, now, time.Hour, hourBuckets).OriginBytes += bytes
}

func (s *Stats) RecordTransform(ms int64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Totals.Transforms++
	s.Totals.TransformMsTotal += ms
	if err != nil {
		s.Totals.TransformErrors++
	}
}

func (s *Stats) prunePaths() {
	list := make([]*PathStat, 0, len(s.Paths))
	for _, p := range s.Paths {
		list = append(list, p)
	}
	// Keep the busiest and most recent; drop a quarter.
	sort.Slice(list, func(i, j int) bool {
		if list[i].Requests != list[j].Requests {
			return list[i].Requests < list[j].Requests
		}
		return list[i].LastSeen.Before(list[j].LastSeen)
	})
	for _, p := range list[:len(list)/4] {
		delete(s.Paths, p.Path)
	}
}

func addCapped(m map[string]int64, key string, capacity int) {
	if _, ok := m[key]; !ok && len(m) >= capacity {
		minKey, minVal := "", int64(-1)
		for k, v := range m {
			if minVal < 0 || v < minVal {
				minKey, minVal = k, v
			}
		}
		delete(m, minKey)
	}
	m[key]++
}

func latencyBucket(ms float64) int {
	for i, b := range latencyBoundsMs {
		if ms <= b {
			return i
		}
	}
	return len(latencyBoundsMs)
}

// percentile estimates from the histogram (upper bound of the bucket).
func percentile(buckets []int64, p float64) float64 {
	var total int64
	for _, c := range buckets {
		total += c
	}
	if total == 0 {
		return 0
	}
	target := int64(float64(total)*p + 0.5)
	var seen int64
	for i, c := range buckets {
		seen += c
		if seen >= target {
			if i < len(latencyBoundsMs) {
				return latencyBoundsMs[i]
			}
			return latencyBoundsMs[len(latencyBoundsMs)-1] * 2
		}
	}
	return 0
}

// bucketFor returns the bucket for now, appending (and trimming) as time moves on.
func bucketFor(list *[]Bucket, now time.Time, size time.Duration, keep int) *Bucket {
	start := now.Truncate(size).Unix()
	l := *list
	if n := len(l); n > 0 && l[n-1].Start == start {
		return &l[n-1]
	}
	l = append(l, Bucket{Start: start})
	if len(l) > keep {
		l = l[len(l)-keep:]
	}
	*list = l
	return &l[len(l)-1]
}

// series returns the last n buckets with empty ones filled in.
func series(list []Bucket, now time.Time, size time.Duration, n int) []Bucket {
	byStart := make(map[int64]Bucket, len(list))
	for _, b := range list {
		byStart[b.Start] = b
	}
	end := now.Truncate(size)
	out := make([]Bucket, n)
	for i := 0; i < n; i++ {
		start := end.Add(-time.Duration(n-1-i) * size).Unix()
		b, ok := byStart[start]
		if !ok {
			b = Bucket{Start: start}
		}
		out[i] = b
	}
	return out
}

func refererParts(ref string) (host, page string) {
	if ref == "" {
		return "(direct)", "(direct)"
	}
	u, err := url.Parse(ref)
	if err != nil || u.Host == "" {
		return "(unknown)", "(unknown)"
	}
	return u.Host, u.Host + u.Path
}

func folderOf(p string) string {
	dir := filepath.Dir(p)
	if dir == "." {
		return "/"
	}
	// Two levels are enough to tell products/12 from sliders.
	parts := strings.SplitN(dir, "/", 3)
	if len(parts) > 2 {
		parts = parts[:2]
	}
	return strings.Join(parts, "/")
}

// ─── Persistence ────────────────────────────────────────────────

func (s *Stats) Save(path string) error {
	if path == "" {
		return nil
	}
	s.mu.Lock()
	data, err := json.Marshal(s)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Stats) Load(path string) {
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("stats: cannot read %s: %v", path, err)
		}
		return
	}
	loaded := NewStats(time.Now())
	if err := json.Unmarshal(data, loaded); err != nil {
		log.Printf("stats: ignoring unreadable %s: %v", path, err)
		return
	}
	if len(loaded.Totals.Latency) != len(latencyBoundsMs)+1 {
		loaded.Totals.Latency = make([]int64, len(latencyBoundsMs)+1)
	}
	if loaded.Totals.Outcomes == nil {
		loaded.Totals.Outcomes = map[string]int64{}
	}
	if loaded.Totals.StatusCodes == nil {
		loaded.Totals.StatusCodes = map[string]int64{}
	}
	if len(loaded.Recent) > recentCapacity || loaded.RecentNext >= recentCapacity {
		loaded.Recent, loaded.RecentNext = nil, 0
	}
	for _, p := range loaded.Paths {
		if p.Referers == nil {
			p.Referers = map[string]int64{}
		}
		if p.Variants == nil {
			p.Variants = map[string]int64{}
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.Since, s.Totals, s.Minutes, s.Hours = loaded.Since, loaded.Totals, loaded.Minutes, loaded.Hours
	s.RefererHosts, s.RefererPages, s.Clients = loaded.RefererHosts, loaded.RefererPages, loaded.Clients
	s.Devices, s.Browsers, s.Bots = loaded.Devices, loaded.Browsers, loaded.Bots
	s.ContentTypes, s.Folders, s.Paths = loaded.ContentTypes, loaded.Folders, loaded.Paths
	s.Recent, s.RecentNext = loaded.Recent, loaded.RecentNext
	s.LastOriginError, s.LastOriginErrorAt = loaded.LastOriginError, loaded.LastOriginErrorAt
	log.Printf("stats: restored %d requests since %s from %s", s.Totals.Requests, s.Since.Format(time.RFC3339), path)
}

// ─── User agents ────────────────────────────────────────────────

var botNames = []struct{ needle, name string }{
	{"googlebot", "Googlebot"}, {"google-inspectiontool", "Google Inspection"}, {"adsbot-google", "Google Ads"},
	{"googleother", "GoogleOther"}, {"bingbot", "Bingbot"}, {"yandex", "Yandex"}, {"baiduspider", "Baidu"},
	{"duckduckbot", "DuckDuckGo"}, {"applebot", "Applebot"}, {"facebookexternalhit", "Facebook preview"},
	{"facebookcatalog", "Facebook catalog"}, {"meta-externalagent", "Meta"}, {"twitterbot", "X / Twitter"},
	{"linkedinbot", "LinkedIn"}, {"slackbot", "Slack"}, {"whatsapp", "WhatsApp preview"},
	{"telegrambot", "Telegram"}, {"discordbot", "Discord"}, {"pinterest", "Pinterest"},
	{"viber", "Viber"}, {"skypeuripreview", "Skype"}, {"ahrefsbot", "Ahrefs"}, {"semrushbot", "Semrush"},
	{"mj12bot", "Majestic"}, {"dotbot", "Moz"}, {"petalbot", "Petal"}, {"bytespider", "ByteDance"},
	{"gptbot", "OpenAI"}, {"chatgpt-user", "ChatGPT"}, {"oai-searchbot", "OpenAI search"},
	{"claudebot", "Claude"}, {"claude-user", "Claude"}, {"perplexity", "Perplexity"}, {"ccbot", "Common Crawl"},
	{"amazonbot", "Amazonbot"}, {"curl/", "curl"}, {"wget/", "wget"}, {"python-requests", "Python"},
	{"go-http-client", "Go client"}, {"node-fetch", "Node"}, {"axios/", "Node"}, {"headlesschrome", "Headless Chrome"},
	{"lighthouse", "Lighthouse"}, {"pagespeed", "PageSpeed"}, {"madan-health-check", "Dashboard health check"},
}

// classifyUA returns a device class (desktop, mobile, tablet, bot) and a browser, app or bot name.
func classifyUA(ua string) (device, browser string) {
	l := strings.ToLower(ua)
	if l == "" {
		return "bot", "No user agent"
	}
	for _, b := range botNames {
		if strings.Contains(l, b.needle) {
			return "bot", b.name
		}
	}
	if strings.Contains(l, "bot") || strings.Contains(l, "crawler") || strings.Contains(l, "spider") {
		return "bot", "Other bot"
	}

	switch {
	case strings.Contains(l, "ipad") || strings.Contains(l, "tablet") || (strings.Contains(l, "android") && !strings.Contains(l, "mobile")):
		device = "tablet"
	case strings.Contains(l, "mobi") || strings.Contains(l, "iphone") || strings.Contains(l, "android"):
		device = "mobile"
	default:
		device = "desktop"
	}

	switch {
	case strings.Contains(l, "fban") || strings.Contains(l, "fbav"):
		browser = "Facebook app"
	case strings.Contains(l, "instagram"):
		browser = "Instagram app"
	case strings.Contains(l, "tiktok") || strings.Contains(l, "musical_ly"):
		browser = "TikTok app"
	case strings.Contains(l, "edg/") || strings.Contains(l, "edga/") || strings.Contains(l, "edgios/"):
		browser = "Edge"
	case strings.Contains(l, "opr/") || strings.Contains(l, "opera"):
		browser = "Opera"
	case strings.Contains(l, "samsungbrowser"):
		browser = "Samsung Internet"
	case strings.Contains(l, "ucbrowser"):
		browser = "UC Browser"
	case strings.Contains(l, "firefox/") || strings.Contains(l, "fxios/"):
		browser = "Firefox"
	case strings.Contains(l, "chrome/") || strings.Contains(l, "crios/"):
		browser = "Chrome"
	case strings.Contains(l, "safari/"):
		browser = "Safari"
	default:
		browser = "Other"
	}
	return device, browser
}
