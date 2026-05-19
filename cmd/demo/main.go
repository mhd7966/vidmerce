// Command demo seeds a running Vidmerce stack over HTTP and writes an HTML
// report with every URL an interviewer can open to verify the project.
//
// Usage (usually via `make demo`):
//
//	go run ./cmd/demo -out .demo/report.html
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const demoPassword = "demo-password-1234"

var (
	baseURL = flag.String("base", envOr("DEMO_BASE_URL", "http://localhost:8080"), "API base URL")
	outPath = flag.String("out", ".demo/report.html", "HTML report path")
)

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "demo seed failed: %v\n", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

type envelope struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
	Meta    json.RawMessage `json:"meta"`
}

type publicUser struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

type tokenPair struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

type video struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	VideoURL    string `json:"video_url"`
	DurationSec int    `json:"duration_sec"`
}

// Real MP4 URLs for feed playback during interviews.
var demoVideoSpecs = []struct {
	title string
	sec   int
	url   string
}{
	{"Demo reel — 10s", 10, "https://samplelib.com/preview/mp4/sample-10s.mp4"},
	{"Demo reel — 15s", 15, "https://samplelib.com/preview/mp4/sample-15s.mp4"},
	{"Demo reel — 30s", 30, "https://test-videos.co.uk/vids/bigbuckbunny/mp4/h264/720/Big_Buck_Bunny_720_10s_1MB.mp4"},
	{"Demo reel — 60s", 60, "https://www.w3schools.com/tags/mov_bbb.mp4"},
	{"Demo short — 4s", 4, "https://samplelib.com/preview/mp4/sample-5s.mp4"},
}

type product struct {
	ID      string `json:"id"`
	VideoID string `json:"video_id"`
	Name    string `json:"name"`
}

type stats struct {
	Views          int64   `json:"views"`
	UniqueViews    int64   `json:"unique_views"`
	Likes          int64   `json:"likes"`
	EngagementRate float64 `json:"engagement_rate"`
}

type reportData struct {
	BaseURL      string
	GeneratedAt  string
	CreatorEmail string
	ViewerEmail  string
	CreatorToken string
	ViewerToken  string
	Videos       []video
	Products     []product
	StatsSample  *stats
	Links        []linkGroup
}

type linkGroup struct {
	Title string
	Items []linkItem
}

type linkItem struct {
	Label string
	URL   string
	Note  string
}

func run() error {
	c := &http.Client{Timeout: 15 * time.Second}

	creatorEmail := "creator@demo.vidmerce.test"
	viewerEmail := "viewer@demo.vidmerce.test"

	if err := register(c, creatorEmail); err != nil {
		return fmt.Errorf("creator register: %w", err)
	}
	creatorTok, err := login(c, creatorEmail)
	if err != nil {
		return fmt.Errorf("creator login: %w", err)
	}

	if err := register(c, viewerEmail); err != nil {
		return fmt.Errorf("viewer register: %w", err)
	}
	viewerTok, err := login(c, viewerEmail)
	if err != nil {
		return fmt.Errorf("viewer login: %w", err)
	}

	var videos []video
	for _, sp := range demoVideoSpecs {
		v, err := createVideo(c, creatorTok, sp.title, sp.url, sp.sec)
		if err != nil {
			return err
		}
		videos = append(videos, v)
	}

	var products []product
	for i := 0; i < 3 && i < len(videos); i++ {
		p, err := createProduct(c, creatorTok, videos[i].ID, fmt.Sprintf("Product for %s", videos[i].Title))
		if err != nil {
			return err
		}
		products = append(products, p)
	}

	// Extra viewers so likes/views show volume on Grafana.
	extraTokens := []string{viewerTok}
	for i := 2; i <= 3; i++ {
		email := fmt.Sprintf("viewer%d@demo.vidmerce.test", i)
		if err := register(c, email); err != nil {
			return err
		}
		tok, err := login(c, email)
		if err != nil {
			return err
		}
		extraTokens = append(extraTokens, tok)
	}

	fmt.Println("Seeding likes, views, rate limits, and filter rejections for Grafana...")
	if err := seedObservabilityTraffic(c, extraTokens, videos); err != nil {
		return err
	}

	fmt.Println("Waiting for worker to flush streams → Postgres/ClickHouse...")
	time.Sleep(6 * time.Second)

	if len(videos) > 0 {
		if err := injectLikeCounterDrift(videos[0].ID); err != nil {
			fmt.Fprintf(os.Stderr, "warn: could not inject reconciler drift (%v)\n", err)
		} else {
			wait := reconcilerWaitDuration()
			fmt.Printf("Waiting %s for like reconciler pass (LIKE_RECONCILER_INTERVAL)...\n", wait)
			time.Sleep(wait)
		}
	}

	var sampleStats *stats
	if len(videos) > 0 {
		s, err := pollStats(c, videos[0].ID, 30)
		if err != nil {
			return fmt.Errorf("stats not ready for demo video %s: %w", videos[0].ID, err)
		}
		sampleStats = &s
		// Warm stats cache + mix (cache_hit / computed) for Grafana.
		for _, v := range videos {
			for i := 0; i < 4; i++ {
				if _, err := getStats(c, v.ID); err != nil {
					fmt.Fprintf(os.Stderr, "warn: stats %s: %v\n", v.ID, err)
				}
			}
		}
	}

	firstVideo := ""
	if len(videos) > 0 {
		firstVideo = videos[0].ID
	}
	firstProduct := ""
	if len(products) > 0 {
		firstProduct = products[0].ID
	}

	base := strings.TrimRight(*baseURL, "/")
	rd := reportData{
		BaseURL:      base,
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		CreatorEmail: creatorEmail,
		ViewerEmail:  viewerEmail,
		CreatorToken: creatorTok,
		ViewerToken:  viewerTok,
		Videos:       videos,
		Products:     products,
		StatsSample:  sampleStats,
		Links: []linkGroup{
			{
				Title: "Platform health & metrics",
				Items: []linkItem{
					{Label: "Swagger UI", URL: base + "/swagger/", Note: "interactive OpenAPI docs"},
					{Label: "API liveness", URL: base + "/health", Note: "should return ok"},
					{Label: "API readiness", URL: base + "/ready", Note: "postgres + redis + clickhouse"},
					{Label: "Prometheus metrics (API)", URL: base + "/metrics", Note: "raw Prometheus text"},
					{Label: "Worker metrics", URL: "http://localhost:9091/metrics", Note: "worker process"},
					{Label: "Prometheus UI", URL: "http://localhost:9090", Note: "targets + queries"},
					{Label: "Grafana dashboards", URL: "http://localhost:3000", Note: "login admin / admin → Vidmerce folder"},
				},
			},
			{
				Title: "Public API (no auth)",
				Items: append([]linkItem{
					{Label: "Feed", URL: base + "/feed?limit=10", Note: "5 seeded videos"},
				}, publicAPILinks(base, firstVideo, firstProduct)...),
			},
		},
	}

	if err := writeReport(rd); err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("══════════════════════════════════════════════════════════")
	fmt.Println("  Vidmerce demo is ready")
	fmt.Println("══════════════════════════════════════════════════════════")
	fmt.Println("  Open report:  file://" + mustAbs(*outPath))
	fmt.Println("  Swagger:      " + rd.BaseURL + "/swagger/")
	fmt.Println("  Grafana:      http://localhost:3000  (admin / admin)")
	fmt.Println("  Prometheus:   http://localhost:9090")
	fmt.Println("  Feed:         " + rd.BaseURL + "/feed?limit=10")
	if firstVideo != "" {
		fmt.Println("  Sample stats: " + rd.BaseURL + "/videos/" + firstVideo + "/stats")
	}
	fmt.Println()
	fmt.Println("  Demo users:")
	fmt.Println("    creator:", creatorEmail, " password:", demoPassword)
	fmt.Println("    viewer: ", viewerEmail, " password:", demoPassword)
	fmt.Println("══════════════════════════════════════════════════════════")
	return nil
}

func publicAPILinks(base, videoID, productID string) []linkItem {
	var out []linkItem
	if videoID != "" {
		out = append(out,
			linkItem{Label: "Get video", URL: base + "/videos/" + videoID, Note: "first seeded video"},
			linkItem{Label: "Video stats", URL: base + "/videos/" + videoID + "/stats", Note: "views, unique_views, likes"},
			linkItem{Label: "Product by video", URL: base + "/videos/" + videoID + "/product", Note: "may 404 if no product"},
		)
	}
	if productID != "" {
		out = append(out, linkItem{Label: "Product by id", URL: base + "/products/" + productID, Note: ""})
	}
	return out
}

func mustAbs(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

func register(c *http.Client, email string) error {
	_, err := post(c, "/auth/register", map[string]string{
		"email": email, "password": demoPassword,
	}, 0, "")
	if err != nil && !strings.Contains(err.Error(), "409") {
		return err
	}
	return nil
}

func login(c *http.Client, email string) (string, error) {
	raw, err := post(c, "/auth/login", map[string]string{
		"email": email, "password": demoPassword,
	}, 0, "")
	if err != nil {
		return "", err
	}
	var tp tokenPair
	if err := json.Unmarshal(raw, &tp); err != nil {
		return "", err
	}
	return tp.AccessToken, nil
}

func createVideo(c *http.Client, token, title, videoURL string, durationSec int) (video, error) {
	raw, err := post(c, "/videos", map[string]any{
		"title":        title,
		"description":  "Seeded by cmd/demo for interview review",
		"video_url":    videoURL,
		"duration_sec": durationSec,
	}, http.StatusCreated, token)
	if err != nil {
		return video{}, err
	}
	var v video
	if err := json.Unmarshal(raw, &v); err != nil {
		return video{}, err
	}
	return v, nil
}

func createProduct(c *http.Client, token, videoID, name string) (product, error) {
	raw, err := post(c, "/products", map[string]any{
		"video_id":    videoID,
		"name":        name,
		"price_cents": 1999,
		"currency":    "USD",
		"image_url":   "https://cdn.example.com/demo/product.png",
	}, http.StatusCreated, token)
	if err != nil {
		return product{}, err
	}
	var p product
	if err := json.Unmarshal(raw, &p); err != nil {
		return product{}, err
	}
	return p, nil
}

func likeVideo(c *http.Client, token, videoID string) error {
	_, err := post(c, "/videos/"+videoID+"/like", nil, http.StatusAccepted, token)
	return err
}

func unlikeVideo(c *http.Client, token, videoID string) error {
	_, err := post(c, "/videos/"+videoID+"/unlike", nil, http.StatusAccepted, token)
	return err
}

func trackView(c *http.Client, token, videoID string, watchMs int) error {
	_, err := post(c, "/videos/"+videoID+"/view", map[string]any{
		"watch_ms": watchMs,
		"country":  "US",
	}, http.StatusAccepted, token)
	return err
}

func getStats(c *http.Client, videoID string) (stats, error) {
	req, err := http.NewRequest(http.MethodGet, *baseURL+"/videos/"+videoID+"/stats", nil)
	if err != nil {
		return stats{}, err
	}
	res, err := c.Do(req)
	if err != nil {
		return stats{}, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		return stats{}, fmt.Errorf("stats %s: %s", res.Status, string(body))
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return stats{}, err
	}
	var s stats
	return s, json.Unmarshal(env.Data, &s)
}

func pollStats(c *http.Client, videoID string, attempts int) (stats, error) {
	var lastErr error
	for i := 0; i < attempts; i++ {
		s, err := getStats(c, videoID)
		if err == nil {
			return s, nil
		}
		lastErr = err
		time.Sleep(time.Second)
	}
	return stats{}, lastErr
}

// seedObservabilityTraffic drives API/worker paths that populate Grafana panels.
func seedObservabilityTraffic(c *http.Client, viewerTokens []string, videos []video) error {
	if len(videos) == 0 {
		return nil
	}

	// Login rate limit (vidmerce_rate_limit_hits_total{bucket="login"}).
	for i := 0; i < 12; i++ {
		_, _ = postStatus(c, "/auth/login", map[string]string{
			"email": "viewer@demo.vidmerce.test", "password": "wrong-password",
		}, 0, "")
	}

	// Likes + unlikes from multiple viewers (like ops, worker apply, stream length).
	for i, v := range videos {
		for j, tok := range viewerTokens {
			if err := likeVideo(c, tok, v.ID); err != nil {
				return fmt.Errorf("like %s: %w", v.ID, err)
			}
			if j == 0 && i%2 == 1 {
				if err := unlikeVideo(c, tok, v.ID); err != nil {
					return fmt.Errorf("unlike %s: %w", v.ID, err)
				}
			}
		}
	}

	// Like rate limit on first video (bucket="like").
	if len(viewerTokens) > 0 {
		vid := videos[0].ID
		tok := viewerTokens[0]
		for i := 0; i < 25; i++ {
			code, err := postStatus(c, "/videos/"+vid+"/like", nil, 0, tok)
			if err != nil {
				return err
			}
			if code == http.StatusTooManyRequests {
				break
			}
		}
	}

	// Views: accepted replays + filter rejections (watch threshold + duration rate).
	for _, v := range videos {
		watchOK := v.DurationSec * 1000 / 3
		if watchOK < 1000 {
			watchOK = 1000
		}
		tok := viewerTokens[0]
		for replay := 0; replay < 3; replay++ {
			if err := trackView(c, tok, v.ID, watchOK); err != nil {
				return fmt.Errorf("view %s: %w", v.ID, err)
			}
		}
		// Below ⅓ duration → watch_threshold:below_threshold.
		if err := trackViewAllowAny(c, tok, v.ID, 50); err != nil {
			return err
		}
		// Burst on a long video → duration_rate:rate_limited (30s video ≈ 2/min cap).
		if v.DurationSec >= 30 {
			for burst := 0; burst < 8; burst++ {
				if err := trackViewAllowAny(c, tok, v.ID, watchOK); err != nil {
					return err
				}
			}
		}
	}

	// Second viewer on first two videos for more stream volume.
	for i := 0; i < 2 && i < len(videos); i++ {
		if len(viewerTokens) < 2 {
			break
		}
		watchMs := videos[i].DurationSec * 1000 / 3
		if watchMs < 1000 {
			watchMs = 1000
		}
		if err := trackView(c, viewerTokens[1], videos[i].ID, watchMs); err != nil {
			return err
		}
	}
	return nil
}

func trackViewAllowAny(c *http.Client, token, videoID string, watchMs int) error {
	code, err := postStatus(c, "/videos/"+videoID+"/view", map[string]any{
		"watch_ms": watchMs,
		"country":  "US",
	}, http.StatusAccepted, token)
	if err != nil {
		return err
	}
	if code != http.StatusAccepted {
		return fmt.Errorf("view %s: unexpected status %d", videoID, code)
	}
	return nil
}

func injectLikeCounterDrift(videoID string) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker not in PATH")
	}
	sql := fmt.Sprintf(
		`UPDATE video_stats SET likes_count = likes_count + 25 WHERE video_id = '%s'`,
		videoID,
	)
	cmd := exec.Command(
		"docker", "exec", "vidmerce-postgres",
		"psql", "-U", "vidmerce", "-d", "vidmerce", "-q", "-c", sql,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func reconcilerWaitDuration() time.Duration {
	raw := os.Getenv("LIKE_RECONCILER_INTERVAL")
	if raw == "" {
		return 35 * time.Second
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 35 * time.Second
	}
	return d + 5*time.Second
}

func postStatus(c *http.Client, path string, payload any, wantStatus int, token string) (int, error) {
	var body io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(*baseURL, "/")+path, body)
	if err != nil {
		return 0, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if wantStatus != 0 && res.StatusCode != wantStatus {
		return res.StatusCode, fmt.Errorf("%s %s: want %d got %d: %s", http.MethodPost, path, wantStatus, res.StatusCode, string(raw))
	}
	return res.StatusCode, nil
}

func post(c *http.Client, path string, payload any, wantStatus int, token string) (json.RawMessage, error) {
	var body io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(*baseURL, "/")+path, body)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if wantStatus != 0 && res.StatusCode != wantStatus {
		return nil, fmt.Errorf("%s %s: want %d got %d: %s", http.MethodPost, path, wantStatus, res.StatusCode, string(raw))
	}
	if res.StatusCode >= 400 {
		return nil, fmt.Errorf("%s %s: %d: %s", http.MethodPost, path, res.StatusCode, string(raw))
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	return env.Data, nil
}

func writeReport(rd reportData) error {
	if err := os.MkdirAll(filepath.Dir(*outPath), 0o755); err != nil {
		return err
	}
	f, err := os.Create(*outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return reportTmpl.Execute(f, rd)
}


var reportTmpl = template.Must(template.New("report").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>Vidmerce demo report</title>
  <style>
    body { font-family: system-ui, sans-serif; max-width: 900px; margin: 2rem auto; padding: 0 1rem; line-height: 1.5; }
    h1 { border-bottom: 2px solid #333; }
    h2 { margin-top: 2rem; color: #444; }
    a { color: #0563c1; }
    table { border-collapse: collapse; width: 100%; margin: 1rem 0; }
    th, td { border: 1px solid #ddd; padding: 0.5rem 0.75rem; text-align: left; }
    th { background: #f5f5f5; }
    .note { color: #666; font-size: 0.9rem; }
    code { background: #f4f4f4; padding: 0.1rem 0.3rem; border-radius: 3px; }
  </style>
</head>
<body>
  <h1>Vidmerce interview demo</h1>
  <p class="note">Generated {{.GeneratedAt}} · API base <code>{{.BaseURL}}</code></p>

  <h2>Quick links</h2>
  {{range .Links}}
  <h3>{{.Title}}</h3>
  <ul>
    {{range .Items}}
    <li><a href="{{.URL}}" target="_blank">{{.Label}}</a>{{if .Note}} — <span class="note">{{.Note}}</span>{{end}}</li>
    {{end}}
  </ul>
  {{end}}

  <h2>Seeded users</h2>
  <table>
    <tr><th>Role</th><th>Email</th><th>Password</th></tr>
    <tr><td>Creator</td><td>{{.CreatorEmail}}</td><td><code>demo-password-1234</code></td></tr>
    <tr><td>Viewer</td><td>{{.ViewerEmail}}</td><td><code>demo-password-1234</code></td></tr>
  </table>

  <h2>Seeded videos</h2>
  <table>
    <tr><th>Title</th><th>ID</th><th>Duration</th><th>Playable URL</th></tr>
    {{range .Videos}}
    <tr>
      <td>{{.Title}}</td>
      <td><code>{{.ID}}</code></td>
      <td>{{.DurationSec}}s</td>
      <td><a href="{{.VideoURL}}" target="_blank">{{.VideoURL}}</a></td>
    </tr>
    {{end}}
  </table>
  <p class="note">Feed items use these URLs — open <a href="{{.BaseURL}}/feed?limit=10">/feed</a> and play from <code>video_url</code>.</p>

  {{if .StatsSample}}
  <h2>Sample stats (first video)</h2>
  <table>
    <tr><th>views</th><th>unique_views</th><th>likes</th><th>engagement_rate</th></tr>
    <tr><td>{{.StatsSample.Views}}</td><td>{{.StatsSample.UniqueViews}}</td><td>{{.StatsSample.Likes}}</td><td>{{printf "%.4f" .StatsSample.EngagementRate}}</td></tr>
  </table>
  {{else}}
  <p class="note">Stats not available yet — wait a few seconds and reload the stats URL above.</p>
  {{end}}

  <h2>Stop demo</h2>
  <p>When finished: <code>make demo-stop</code> (stops API + worker started by <code>make demo</code>).</p>
</body>
</html>
`))
