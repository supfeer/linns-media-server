package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testYouTubeSession = `[{"domain":".youtube.com","name":"SAPISID","value":"test-session","path":"/","secure":true}]`

func TestYouTubeBrowserSessionScope(t *testing.T) {
	cookies, err := youtubeBrowserSession([]byte(testYouTubeSession))
	if err != nil || !strings.Contains(string(cookies), ".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\ttest-session") {
		t.Fatalf("valid session: %v", err)
	}
	for _, data := range []string{
		`[]`, `{}`, strings.Repeat("x", youtubeSessionLimit+1),
		strings.ReplaceAll(testYouTubeSession, ".youtube.com", ".google.com"),
		strings.ReplaceAll(testYouTubeSession, "test-session", `bad\nvalue`),
		strings.ReplaceAll(testYouTubeSession, `"secure":true`, `"secure":true,"expirationDate":1`),
	} {
		if _, err := youtubeBrowserSession([]byte(data)); err == nil {
			t.Fatal("accepted invalid or unrelated cookies")
		}
	}
}

func TestYouTubeBrowserConnection(t *testing.T) {
	bridge := testYouTubeBridge(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var endpoint string
	err := bridge.connectBrowser(ctx, "<extension folder>", func(u *url.URL) error {
		if u.Hostname() != "127.0.0.1" || len(u.Fragment) != 64 {
			t.Fatal("login must stay on loopback with token")
		}
		endpoint = "http://" + u.Host
		page, err := http.Get(endpoint + u.Path)
		if err != nil {
			return err
		}
		body, err := io.ReadAll(page.Body)
		page.Body.Close()
		if err != nil {
			return err
		}
		if strings.Contains(string(body), u.Fragment) || strings.Contains(string(body), "<extension folder>") {
			t.Fatal("page leaked token or unescaped input")
		}
		for _, check := range []struct {
			origin, token, host, data string
			code                      int
		}{
			{"https://evil.test", u.Fragment, "", testYouTubeSession, 403},
			{"chrome-extension://" + strings.Repeat("a", 32), "wrong", "", testYouTubeSession, 403},
			{"chrome-extension://" + strings.Repeat("a", 32), u.Fragment, "evil.test", testYouTubeSession, 403},
			{"chrome-extension://" + strings.Repeat("a", 32), u.Fragment, "", `[]`, 400},
			{"chrome-extension://" + strings.Repeat("a", 32), u.Fragment, "", testYouTubeSession, 204},
			{"chrome-extension://" + strings.Repeat("a", 32), u.Fragment, "", testYouTubeSession, 409},
		} {
			request, _ := http.NewRequest(http.MethodPost, endpoint+"/youtube-session", strings.NewReader(check.data))
			request.Header.Set("Origin", check.origin)
			request.Header.Set("Authorization", "Bearer "+check.token)
			if check.host != "" {
				request.Host = check.host
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				return err
			}
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != check.code {
				t.Errorf("expected %d, got %d", check.code, response.StatusCode)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bridge.hasSession() {
		t.Fatal("session missing")
	}
	if len(bridge.feeds) != 0 {
		t.Fatal("old account lists retained")
	}
	info, err := os.Stat(filepath.Join(bridge.root, youtubeAccountFile))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("session permissions: %v", err)
	}
	restarted, err := newYouTubeBridge(t.Context(), bridge.root)
	if err != nil || !restarted.hasSession() {
		t.Fatalf("session not restored: %v", err)
	}
	if response, err := http.Get(endpoint + "/youtube-connect"); err == nil {
		response.Body.Close()
		t.Fatal("login listener stayed open")
	}
}

func TestYouTubeBrowserCancel(t *testing.T) {
	bridge := testYouTubeBridge(t)
	ctx, cancel := context.WithCancel(context.Background())
	var endpoint string
	err := bridge.connectBrowser(ctx, "", func(u *url.URL) error { endpoint = "http://" + u.Host; cancel(); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if bridge.hasSession() {
		t.Fatal("cancellation saved account")
	}
	if response, err := http.Get(endpoint); err == nil {
		response.Body.Close()
		t.Fatal("canceled listener stayed open")
	}
}
