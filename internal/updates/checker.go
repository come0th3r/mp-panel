package updates

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"
)

const (
	releaseAPI      = "https://api.github.com/repos/come0th3r/mp-panel/releases/latest"
	repositoryURL   = "https://github.com/come0th3r/mp-panel"
	successTTL      = 6 * time.Hour
	failureTTL      = 15 * time.Minute
	minimumInterval = time.Minute
	requestTimeout  = 4 * time.Second
	responseLimit   = 1 << 20
)

type Status struct {
	CurrentVersion  string `json:"current_version"`
	LatestVersion   string `json:"latest_version,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
	ReleaseURL      string `json:"release_url,omitempty"`
	PublishedAt     string `json:"published_at,omitempty"`
	CheckedAt       string `json:"checked_at,omitempty"`
	Error           string `json:"error,omitempty"`
}

type Checker struct {
	mu          sync.Mutex
	client      *http.Client
	status      Status
	lastAttempt time.Time
	nextCheck   time.Time
	inFlight    chan struct{}
}

func New(current string) *Checker {
	return &Checker{
		status: Status{CurrentVersion: current},
		client: &http.Client{
			Timeout: requestTimeout,
			// The endpoint is fixed. Never follow a redirect or include
			// installation details in the User-Agent.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Cached never waits for GitHub. An expired cache starts one background check.
func (c *Checker) Cached() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.startLocked(false)
	return c.status
}

// Check coalesces manual checks and limits GitHub requests to one per minute.
// Failures retain previously validated release details.
func (c *Checker) Check(ctx context.Context) Status {
	c.mu.Lock()
	done := c.startLocked(true)
	c.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// Caller holds mu. Checks own their timeout independently of browser requests.
func (c *Checker) startLocked(force bool) chan struct{} {
	if c.inFlight != nil {
		return c.inFlight
	}
	now := time.Now()
	if now.Sub(c.lastAttempt) < minimumInterval || (!force && now.Before(c.nextCheck)) {
		return nil
	}
	c.lastAttempt = now
	done := make(chan struct{})
	c.inFlight = done
	go c.refresh(done)
	return done
}

func (c *Checker) refresh(done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	latest, err := c.fetch(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now().UTC()
	c.status.CheckedAt = now.Format(time.RFC3339)
	if err != nil {
		c.status.Error = err.Error()
		c.nextCheck = now.Add(failureTTL)
	} else {
		c.status.LatestVersion = latest.tag
		c.status.ReleaseURL = latest.url
		c.status.PublishedAt = latest.published
		c.status.UpdateAvailable = newer(latest.tag, c.status.CurrentVersion)
		c.status.Error = ""
		c.nextCheck = now.Add(successTTL)
	}
	c.inFlight = nil
	close(done)
}

type release struct{ tag, url, published string }

func (c *Checker) fetch(ctx context.Context) (release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseAPI, nil)
	if err != nil {
		return release{}, errors.New("Не удалось подготовить проверку обновлений")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	req.Header.Set("User-Agent", "MP-Panel-Update-Checker")
	resp, err := c.client.Do(req)
	if err != nil {
		return release{}, errors.New("Не удалось связаться с GitHub. Попробуйте проверить позже")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return release{}, errors.New("Опубликованный релиз пока не найден на GitHub")
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		return release{}, errors.New("GitHub временно ограничил проверки обновлений. Попробуйте позже")
	}
	if resp.StatusCode != http.StatusOK {
		return release{}, errors.New("GitHub не смог вернуть сведения о релизе. Попробуйте позже")
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, responseLimit+1))
	if err != nil || len(b) > responseLimit {
		return release{}, errors.New("Не удалось прочитать сведения о релизе GitHub")
	}
	var raw struct {
		Tag        string `json:"tag_name"`
		URL        string `json:"html_url"`
		Published  string `json:"published_at"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if json.Unmarshal(b, &raw) != nil || raw.Draft || raw.Prerelease {
		return release{}, errors.New("GitHub вернул неподходящие сведения о релизе")
	}
	if _, ok := parseStable(raw.Tag); !ok || raw.URL != repositoryURL+"/releases/tag/"+raw.Tag {
		return release{}, errors.New("Не удалось проверить версию или адрес релиза GitHub")
	}
	published, err := time.Parse(time.RFC3339, raw.Published)
	if err != nil {
		return release{}, errors.New("Не удалось проверить дату публикации релиза GitHub")
	}
	return release{raw.Tag, raw.URL, published.UTC().Format(time.RFC3339)}, nil
}

var stableVersion = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

// Build metadata does not affect precedence. Prerelease tags are excluded.
func parseStable(value string) ([3]uint64, bool) {
	var parts [3]uint64
	if len(value) > 128 {
		return parts, false
	}
	matches := stableVersion.FindStringSubmatch(value)
	if matches == nil {
		return parts, false
	}
	for i := range parts {
		n, err := strconv.ParseUint(matches[i+1], 10, 64)
		if err != nil {
			return parts, false
		}
		parts[i] = n
	}
	return parts, true
}

func newer(latest, current string) bool {
	l, lok := parseStable(latest)
	c, cok := parseStable(current)
	if !lok || !cok {
		return false
	}
	for i := range l {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}
