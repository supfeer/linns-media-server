package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

//go:embed youtube-extension/*
var youtubeExtension embed.FS

const youtubeAccountFile = ".youtube-account.json"
const youtubeSessionLimit = 512 << 10

type youtubeBrowserCookie struct {
	Domain     string  `json:"domain"`
	Name       string  `json:"name"`
	Value      string  `json:"value"`
	Path       string  `json:"path"`
	Secure     bool    `json:"secure"`
	Expiration float64 `json:"expirationDate,omitempty"`
}

func youtubeBrowserSession(data []byte) ([]byte, error) {
	if len(data) > youtubeSessionLimit {
		return nil, errors.New("session too large")
	}
	var cookies []youtubeBrowserCookie
	if err := json.Unmarshal(data, &cookies); err != nil {
		return nil, errors.New("invalid session")
	}
	var output strings.Builder
	output.WriteString("# Netscape HTTP Cookie File\n")
	authenticated := false
	for _, cookie := range cookies {
		if cookie.Domain != ".youtube.com" && cookie.Domain != "youtube.com" && cookie.Domain != "www.youtube.com" {
			return nil, errors.New("only YouTube cookies are accepted")
		}
		if cookie.Name == "" || !strings.HasPrefix(cookie.Path, "/") || strings.ContainsAny(cookie.Name+cookie.Value+cookie.Path, "\t\r\n\x00") || cookie.Expiration < 0 || cookie.Expiration > 1e12 {
			return nil, errors.New("invalid cookie")
		}
		if cookie.Expiration > 0 && cookie.Expiration <= float64(time.Now().Unix()) {
			continue
		}
		if (cookie.Name == "SAPISID" || cookie.Name == "__Secure-3PAPISID") && cookie.Value != "" {
			authenticated = true
		}
		domain, secure := "FALSE", "FALSE"
		if strings.HasPrefix(cookie.Domain, ".") {
			domain = "TRUE"
		}
		if cookie.Secure {
			secure = "TRUE"
		}
		fmt.Fprintf(&output, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", cookie.Domain, domain, cookie.Path, secure, int64(cookie.Expiration), cookie.Name, cookie.Value)
	}
	if !authenticated {
		return nil, errors.New("sign in to YouTube first")
	}
	return []byte(output.String()), nil
}

func (bridge *youtubeBridge) saveBrowserSession(data []byte) error {
	cookies, err := youtubeBrowserSession(data)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(bridge.root, ".youtube-session-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), filepath.Join(bridge.root, youtubeAccountFile)); err != nil {
		return err
	}
	bridge.mu.Lock()
	bridge.cancelPrefetchExcept("")
	bridge.session = cookies
	bridge.feeds = make(map[string][]youtubeVideo)
	bridge.resolved = make(map[string]youtubeResolved)
	bridge.revision++
	// A reconnect must not restore lists belonging to the previous session.
	_ = os.Remove(filepath.Join(bridge.root, youtubeCatalogFile))
	bridge.mu.Unlock()
	return nil
}

func (bridge *youtubeBridge) hasSession() bool {
	bridge.mu.RLock()
	defer bridge.mu.RUnlock()
	return len(bridge.session) > 0
}

func installYouTubeExtension() (string, error) {
	config, err := appConfigPath()
	if err != nil {
		return "", err
	}
	directory := filepath.Join(filepath.Dir(config), "YouTube browser extension")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", err
	}
	entries, err := youtubeExtension.ReadDir("youtube-extension")
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		data, err := youtubeExtension.ReadFile("youtube-extension/" + entry.Name())
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(directory, entry.Name()), data, 0600); err != nil {
			return "", err
		}
	}
	return directory, nil
}

var youtubeExtensionOrigin = regexp.MustCompile(`^chrome-extension://[a-p]{32}$`)
var youtubeConnectPage = template.Must(template.New("connect").Parse(`<!doctype html>
<html lang="{{.Language}}"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Linn — YouTube</title><style>body{font:18px/1.6 system-ui;max-width:680px;margin:60px auto;padding:24px;color:#222}h1{line-height:1.2}code{display:block;overflow-wrap:anywhere;padding:16px;background:#f3f4f6;font-size:14px}a{color:#145ec7}details{margin-top:36px}summary{cursor:pointer}</style>
<h1>{{.Title}}</h1><p>{{.Description}}</p><ol><li><a href="https://www.youtube.com/" target="_blank" rel="noreferrer">{{.SignIn}}</a></li><li>{{.Connect}}</li></ol><p>{{.Privacy}}</p>
<details open><summary>{{.SetupTitle}}</summary><p>{{.Setup}}</p><code>{{.Path}}</code></details></html>`))

func (bridge *youtubeBridge) connectBrowser(ctx context.Context, extensionDirectory string, open func(*url.URL) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		listener.Close()
		return err
	}
	token := hex.EncodeToString(random[:])
	result := make(chan error, 1)
	var mu sync.Mutex
	used := false
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	server.BaseContext = func(net.Listener) context.Context { return ctx }
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Host != listener.Addr().String() {
			http.Error(w, "Invalid host", 403)
			return
		}
		if r.URL.Path == "/youtube-connect" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
			_ = youtubeConnectPage.Execute(w, map[string]string{
				"Language": tr("linn.youtube.browser_language"), "Title": tr("linn.youtube.browser_title"),
				"Description": tr("linn.youtube.browser_description"), "SignIn": tr("linn.youtube.browser_signin"),
				"Connect": tr("linn.youtube.browser_connect"), "Privacy": tr("linn.youtube.browser_privacy"),
				"SetupTitle": tr("linn.youtube.browser_setup_title"), "Setup": tr("linn.youtube.browser_setup"), "Path": extensionDirectory,
			})
			return
		}
		if r.URL.Path != "/youtube-session" {
			http.NotFound(w, r)
			return
		}
		origin := r.Header.Get("Origin")
		if !youtubeExtensionOrigin.MatchString(origin) {
			http.Error(w, "Extension required", 403)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "POST")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "Invalid connection", 403)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if ctx.Err() != nil {
			http.Error(w, "Connection expired", http.StatusGone)
			return
		}
		if used {
			http.Error(w, "Connection already completed", 409)
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, youtubeSessionLimit))
		if err != nil {
			http.Error(w, "Invalid session", 400)
			return
		}
		if _, err := youtubeBrowserSession(data); err != nil {
			http.Error(w, "Sign in to YouTube and retry", 400)
			return
		}
		if err := bridge.saveBrowserSession(data); err != nil {
			http.Error(w, "Cannot save session", 500)
			return
		}
		used = true
		w.WriteHeader(http.StatusNoContent)
		result <- nil
	})
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			select {
			case result <- err:
			default:
			}
		}
	}()
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
		_ = server.Close()
	}()
	u := &url.URL{Scheme: "http", Host: listener.Addr().String(), Path: "/youtube-connect", Fragment: token}
	if err := open(u); err != nil {
		return err
	}
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
