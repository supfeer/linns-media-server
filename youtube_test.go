package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testYouTubeBridge(t *testing.T) *youtubeBridge {
	t.Helper()
	bridge, err := newYouTubeBridge(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bridge.feeds["recommended"] = []youtubeVideo{{ID: "jNQXAC9IVRw", Title: "Test & video", Duration: 19}}
	return bridge
}

func TestYouTubeStreamFormats(t *testing.T) {
	for _, test := range []struct {
		name, json string
		count      int
	}{
		{"combined", `{"vcodec":"avc1.640028","acodec":"mp4a.40.2","url":"https://r.googlevideo.com/video"}`, 1},
		{"separate", `{"requested_formats":[{"vcodec":"avc1.640028","url":"https://r.googlevideo.com/video"},{"url":"https://r.googlevideo.com/audio"}]}`, 2},
		{"missing", `{}`, 0},
		{"unrelated host", `{"vcodec":"avc1.640028","acodec":"mp4a.40.2","url":"https://evil.test/video"}`, 0},
		{"file", `{"vcodec":"avc1.640028","acodec":"mp4a.40.2","url":"file:///etc/passwd"}`, 0},
		{"userinfo", `{"vcodec":"avc1.640028","acodec":"mp4a.40.2","url":"https://user@r.googlevideo.com/video"}`, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			urls, err := parseYouTubeStream([]byte(test.json))
			if test.count == 0 && err == nil {
				t.Fatal("accepted invalid formats")
			}
			if test.count != 0 && (err != nil || len(urls.URLs) != test.count) {
				t.Fatalf("formats: %v %v", urls, err)
			}
		})
	}
}

func TestYouTubeCatalogAndLazyHead(t *testing.T) {
	bridge := testYouTubeBridge(t)
	source := bridge.source()
	catalog, err := newCatalog([]librarySource{source})
	if err != nil {
		t.Fatal(err)
	}
	server := newMediaServer("Test", "test", "127.0.0.1:1339", net.IPv4(127, 0, 0, 1), catalog)
	server.youtube = bridge
	entries, ok := bridge.entries(source.ID+"/recommended", false)
	if !ok || len(entries) != 1 {
		t.Fatalf("entries: %#v", entries)
	}
	data, err := server.marshalDIDL(entries)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("/youtube/jNQXAC9IVRw.ts")) || bytes.Contains(data, []byte(`size="0"`)) || !bytes.Contains(data, []byte("DLNA.ORG_OP=00")) {
		t.Fatalf("invalid lazy resource: %s", data)
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		_, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	// A TV can probe HEAD while browsing. No tools, network calls or downloads may start.
	t.Setenv("PATH", t.TempDir())
	request := httptest.NewRequest(http.MethodHead, "/youtube/jNQXAC9IVRw.ts", nil)
	response := httptest.NewRecorder()
	bridge.serve(response, request)
	if response.Code != 200 || response.Body.Len() != 0 || response.Header().Get("Content-Type") != "video/mp2t" {
		t.Fatalf("HEAD: %d %s", response.Code, response.Body.String())
	}
	files, err := os.ReadDir(bridge.root)
	if err != nil || len(files) != 0 {
		t.Fatalf("browsing wrote files: %v %v", files, err)
	}
	for _, url := range []string{"/youtube/../../etc/passwd.ts", "/youtube/abcdefghijk.ts"} {
		request := httptest.NewRequest(http.MethodGet, url, nil)
		response := httptest.NewRecorder()
		bridge.serve(response, request)
		if response.Code != 404 {
			t.Fatalf("accepted unknown video %s: %d", url, response.Code)
		}
	}
	request = httptest.NewRequest(http.MethodGet, "/youtube/jNQXAC9IVRw.ts", nil)
	request.Header.Set("Range", "bytes=500-")
	response = httptest.NewRecorder()
	bridge.serve(response, request)
	if response.Code != 416 {
		t.Fatalf("unexpected seek response: %d", response.Code)
	}
}

func TestYouTubeStreamingIntegration(t *testing.T) {
	if os.Getenv("LINN_YOUTUBE_STREAM_TEST") != "1" {
		t.Skip("set LINN_YOUTUBE_STREAM_TEST=1")
	}
	bridge := testYouTubeBridge(t)
	if os.Getenv("LINN_YOUTUBE_ACCOUNT_TEST") == "1" {
		cache, err := os.UserCacheDir()
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(cache, "FolderDLNA", "YouTube", youtubeAccountFile))
		if err != nil {
			t.Fatal(err)
		}
		session, err := youtubeBrowserSession(data)
		if err != nil {
			t.Fatal(err)
		}
		bridge.session = session
	}
	server := httptest.NewServer(http.HandlerFunc(bridge.serve))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/youtube/jNQXAC9IVRw.ts", nil)
	started := time.Now()
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("stream status %d: %s", response.StatusCode, body)
	}
	packet := make([]byte, 188)
	if _, err := io.ReadFull(response.Body, packet); err != nil {
		t.Fatal(err)
	}
	if packet[0] != 0x47 {
		t.Fatal("not MPEG-TS")
	}
	t.Logf("First MPEG-TS packet after %s", time.Since(started))
	n, err := io.Copy(io.Discard, response.Body)
	if err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(bridge.root)
	if err != nil || len(files) != 0 {
		t.Fatalf("stream wrote files: %v %v", files, err)
	}
	t.Logf("Streamed %d bytes without writing media to disk", n+188)
}

func TestYouTubeAccountIntegration(t *testing.T) {
	if os.Getenv("LINN_YOUTUBE_ACCOUNT_TEST") != "1" {
		t.Skip("set LINN_YOUTUBE_ACCOUNT_TEST=1")
	}
	bridge := testYouTubeBridge(t)
	bridge.feeds = make(map[string][]youtubeVideo)
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(cache, "FolderDLNA", "YouTube", youtubeAccountFile))
	if err != nil {
		t.Fatal(err)
	}
	bridge.session, err = youtubeBrowserSession(data)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if err := bridge.refresh(ctx, nil); err != nil {
		t.Fatal(err)
	}
	for _, feed := range youtubeFeeds {
		t.Logf("%s: %d videos", feed.ID, len(bridge.feeds[feed.ID]))
	}
	if len(bridge.feeds["recommended"]) == 0 {
		t.Fatal("personal recommendations are empty")
	}
	files, err := os.ReadDir(bridge.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasPrefix(filepath.Base(file.Name()), ".youtube-session-") {
			t.Fatal("session file was not removed")
		}
	}
}

func TestYouTubeRefreshProgressAndCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	bridge := testYouTubeBridge(t)
	bridge.session = []byte("# Netscape HTTP Cookie File\n")
	tools := t.TempDir()
	script := `#!/bin/sh
printf '%s' '{"entries":[{"id":"jNQXAC9IVRw","title":"Ready"},{"id":"jNQXAC9IVRw","title":"Duplicate"},{"id":"../bad","title":"Invalid"}]}'
`
	for _, tool := range []string{"node", "yt-dlp"} {
		if err := os.WriteFile(filepath.Join(tools, tool), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", tools)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var steps []int
	err := bridge.refresh(ctx, func(step int, key string) {
		steps = append(steps, step)
		if step == 2 {
			entries, _ := bridge.entries(bridge.source().ID+"/recommended", false)
			if len(entries) != 1 || entries[0].Name != "Ready" {
				t.Errorf("first list not published: %#v", entries)
			}
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if len(steps) != 3 || steps[0] != 0 || steps[1] != 1 || steps[2] != 2 {
		t.Fatalf("progress: %v", steps)
	}
	files, err := os.ReadDir(bridge.root)
	if err != nil || len(files) != 1 || files[0].Name() != youtubeCatalogFile {
		t.Fatalf("session not cleaned: %v %v", files, err)
	}
	steps = nil
	if err := bridge.refresh(context.Background(), func(step int, _ string) { steps = append(steps, step) }); err != nil {
		t.Fatal(err)
	}
	if len(steps) != 6 || steps[5] != 5 {
		t.Fatalf("retry progress: %v", steps)
	}
	for _, feed := range youtubeFeeds {
		if len(bridge.feeds[feed.ID]) != 1 {
			t.Fatalf("invalid list %s", feed.ID)
		}
	}
}

func TestYouTubeRefreshKeepsLastGoodLists(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	for _, check := range []struct {
		name, output, warning string
	}{
		{"empty recommendations", `{"entries":[]}`, ""},
		{"missing entries", `{}`, ""},
		{"expired session", `{"entries":[]}`, "WARNING: The provided YouTube account cookies are no longer valid."},
	} {
		t.Run(check.name, func(t *testing.T) {
			bridge := testYouTubeBridge(t)
			bridge.session = []byte("test-session")
			bridge.feeds["later"] = append([]youtubeVideo(nil), bridge.feeds["recommended"]...)
			tools := t.TempDir()
			script := "#!/bin/sh\nprintf '%s' '" + check.output + "'\nprintf '%s' '" + check.warning + "' >&2\n"
			for _, tool := range []string{"node", "yt-dlp"} {
				if err := os.WriteFile(filepath.Join(tools, tool), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", tools)
			err := bridge.refresh(context.Background(), nil)
			if err == nil || len(bridge.feeds["recommended"]) != 1 {
				t.Fatalf("lost last good recommendations: %v", err)
			}
			if check.name == "empty recommendations" {
				if len(bridge.feeds["later"]) != 0 {
					t.Fatal("legitimately emptied Watch later was retained")
				}
			} else if len(bridge.feeds["later"]) != 1 || bridge.version() != 0 {
				t.Fatal("failed refresh changed the published library")
			}
			if check.warning != "" && err.Error() != "linn.youtube.account_error" {
				t.Fatalf("expired session not reported: %v", err)
			}
			version := bridge.version()
			_ = bridge.refresh(context.Background(), nil)
			if bridge.version() != version {
				t.Fatal("unchanged lists published again")
			}
		})
	}
}

func TestYouTubeCatalogSurvivesRestartButNotAccountChange(t *testing.T) {
	bridge := testYouTubeBridge(t)
	if err := bridge.saveBrowserSession([]byte(testYouTubeSession)); err != nil {
		t.Fatal(err)
	}
	bridge.feeds["recommended"] = []youtubeVideo{{ID: "jNQXAC9IVRw", Title: "Saved video"}}
	if err := bridge.saveCatalog(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(bridge.root, youtubeCatalogFile)
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0600 || info.Size() > 1024 {
		t.Fatalf("metadata cache permissions/size: %v", err)
	}
	restarted, err := newYouTubeBridge(t.Context(), bridge.root)
	if err != nil || len(restarted.feeds["recommended"]) != 1 {
		t.Fatalf("last good list not restored: %v", err)
	}
	oldCache, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.saveBrowserSession([]byte(strings.ReplaceAll(testYouTubeSession, "test-session", "new-session"))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("previous session cache retained")
	}
	// Even a leftover cache must not cross account boundaries.
	if err := os.WriteFile(file, oldCache, 0600); err != nil {
		t.Fatal(err)
	}
	restarted, err = newYouTubeBridge(t.Context(), bridge.root)
	if err != nil || len(restarted.feeds) != 0 {
		t.Fatalf("another account's list restored: %v", err)
	}
}

func TestYouTubeAccountChangeDuringResolution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	bridge := testYouTubeBridge(t)
	bridge.session = []byte("old-session")
	tools := t.TempDir()
	marker := filepath.Join(tools, "started")
	script := "#!/bin/sh\nprintf x > '" + marker + "'\n/bin/sleep 0.2\nprintf '%s' '{\"requested_formats\":[{\"vcodec\":\"avc1.640028\",\"url\":\"https://r.test.googlevideo.com/video\"},{\"url\":\"https://r.test.googlevideo.com/audio\"}]}'\n"
	for _, tool := range []string{"node", "yt-dlp"} {
		if err := os.WriteFile(filepath.Join(tools, tool), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", tools)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := bridge.resolve(ctx, "jNQXAC9IVRw"); done <- err }()
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("extractor did not start")
		}
		time.Sleep(time.Millisecond)
	}
	if err := bridge.saveBrowserSession([]byte(testYouTubeSession)); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	bridge.mu.RLock()
	defer bridge.mu.RUnlock()
	if len(bridge.resolved) != 0 {
		t.Fatal("old account URLs cached after account changed")
	}
}

func TestYouTubeCacheIsNotFilesystemCatalog(t *testing.T) {
	bridge := testYouTubeBridge(t)
	sources := []librarySource{bridge.source()}
	before, err := scanSources(sources, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".youtube-session-test", "unexpected.mp4"} {
		if err := os.WriteFile(filepath.Join(bridge.root, name), []byte("test"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	after, err := scanSources(sources, true)
	if err != nil {
		t.Fatal(err)
	}
	if !sameEntries(before, after) {
		t.Fatal("YouTube cache writes changed the published filesystem catalog")
	}
}
