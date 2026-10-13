package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

var youtubeID = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
var youtubeFeeds = []struct{ ID, Key, URL string }{
	{"recommended", "linn.youtube.recommended", ":ytrec"},
	{"subscriptions", "linn.youtube.subscriptions", ":ytsubs"},
	{"later", "linn.youtube.later", ":ytwatchlater"},
	{"liked", "linn.youtube.liked", "https://www.youtube.com/playlist?list=LL"},
}

type youtubeVideo struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	Duration float64 `json:"duration"`
}

type youtubeBridge struct {
	ctx          context.Context
	root         string
	mu           sync.RWMutex
	refreshMu    sync.Mutex
	resolveMu    sync.Mutex
	feeds        map[string][]youtubeVideo
	updated      time.Time
	revision     uint32
	streams      chan struct{}
	resolved     map[string]youtubeResolved
	prefetch     map[string]*youtubePrefetch
	playback     youtubePlaybackSettings
	session      []byte
	searchSlot   chan struct{}
	searches     map[string]youtubeSearchResult
	searchStatus string
}

type youtubePrefetch struct {
	cancel  context.CancelFunc
	expires time.Time
}

type youtubeResolved struct {
	stream  youtubeStream
	expires time.Time
}

const youtubeCatalogFile = ".youtube-lists.json"

type youtubeCatalog struct {
	Session [32]byte                  `json:"session"`
	Feeds   map[string][]youtubeVideo `json:"feeds"`
}

func newYouTubeBridge(ctx context.Context, root string) (*youtubeBridge, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), ".youtube-session-") {
			if err := os.Remove(filepath.Join(root, entry.Name())); err != nil {
				return nil, err
			}
		}
	}
	bridge := &youtubeBridge{ctx: ctx, root: root, feeds: make(map[string][]youtubeVideo), streams: make(chan struct{}, 2), resolved: make(map[string]youtubeResolved), prefetch: make(map[string]*youtubePrefetch)}
	bridge.playback, _ = (youtubePlaybackSettings{}).normalized()
	bridge.searchSlot = make(chan struct{}, 1)
	bridge.searches = make(map[string]youtubeSearchResult)
	if file, err := os.Open(filepath.Join(root, youtubeAccountFile)); err == nil {
		data, readErr := io.ReadAll(io.LimitReader(file, youtubeSessionLimit+1))
		file.Close()
		if readErr == nil {
			bridge.session, _ = youtubeBrowserSession(data)
		}
	}
	if len(bridge.session) > 0 {
		if file, err := os.Open(filepath.Join(root, youtubeCatalogFile)); err == nil {
			var catalog youtubeCatalog
			err := json.NewDecoder(io.LimitReader(file, 2<<20)).Decode(&catalog)
			file.Close()
			if err == nil && catalog.Session == sha256.Sum256(bridge.session) {
				for _, feed := range youtubeFeeds {
					bridge.feeds[feed.ID] = catalog.Feeds[feed.ID]
				}
			}
		}
	}
	return bridge, nil
}

// Caller holds mu so an old account cannot overwrite the current account's lists.
func (bridge *youtubeBridge) saveCatalog() error {
	data, err := json.Marshal(youtubeCatalog{Session: sha256.Sum256(bridge.session), Feeds: bridge.feeds})
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
	return os.Rename(file.Name(), filepath.Join(bridge.root, youtubeCatalogFile))
}

func (bridge *youtubeBridge) source() librarySource {
	return librarySource{ID: stableUUID(bridge.root), Name: "YouTube", Path: bridge.root, Virtual: true}
}

func (bridge *youtubeBridge) owns(rel string) bool {
	root := bridge.source().ID
	return rel == root || strings.HasPrefix(rel, root+"/")
}

func (bridge *youtubeBridge) entries(rel string, metadata bool) ([]mediaEntry, bool) {
	bridge.mu.RLock()
	defer bridge.mu.RUnlock()
	root := bridge.source().ID
	if rel == root+"/search" || strings.HasPrefix(rel, root+"/search/") {
		return bridge.searchEntries(rel, metadata)
	}
	if rel == root {
		if metadata {
			return []mediaEntry{{Rel: root, Name: "YouTube", Dir: true}}, true
		}
		entries := make([]mediaEntry, 0, len(youtubeFeeds))
		for _, feed := range youtubeFeeds {
			entries = append(entries, mediaEntry{Rel: root + "/" + feed.ID, Name: tr(feed.Key), Dir: true})
		}
		return entries, true
	}
	for _, feed := range youtubeFeeds {
		parent := root + "/" + feed.ID
		if rel == parent {
			if metadata {
				return []mediaEntry{{Rel: parent, Name: tr(feed.Key), Dir: true}}, true
			}
			entries := make([]mediaEntry, 0, len(bridge.feeds[feed.ID]))
			for _, video := range bridge.feeds[feed.ID] {
				entries = append(entries, mediaEntry{Rel: parent + "/" + video.ID, Name: video.Title})
			}
			return entries, true
		}
		if metadata && strings.HasPrefix(rel, parent+"/") {
			id := strings.TrimPrefix(rel, parent+"/")
			for _, video := range bridge.feeds[feed.ID] {
				if video.ID == id {
					return []mediaEntry{{Rel: rel, Name: video.Title}}, true
				}
			}
		}
	}
	return nil, false
}

func (bridge *youtubeBridge) video(id string) (youtubeVideo, bool) {
	bridge.mu.RLock()
	defer bridge.mu.RUnlock()
	for _, result := range bridge.searches {
		for _, video := range result.Videos {
			if video.ID == id {
				return video, true
			}
		}
	}
	for _, videos := range bridge.feeds {
		for _, video := range videos {
			if video.ID == id {
				return video, true
			}
		}
	}
	return youtubeVideo{}, false
}

func (bridge *youtubeBridge) version() uint32 {
	bridge.mu.RLock()
	defer bridge.mu.RUnlock()
	return bridge.revision
}

func (bridge *youtubeBridge) refresh(ctx context.Context, progress func(int, string)) error {
	if !bridge.refreshMu.TryLock() {
		return errors.New("linn.youtube.refreshing")
	}
	defer bridge.refreshMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if progress == nil {
		progress = func(int, string) {}
	}
	progress(0, "")
	bridge.mu.RLock()
	cookies := append([]byte(nil), bridge.session...)
	bridge.mu.RUnlock()
	if len(cookies) == 0 {
		return errors.New("linn.youtube.account_error")
	}
	file, err := os.CreateTemp(bridge.root, ".youtube-session-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(cookies); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	args, err := youtubeArguments()
	if err != nil {
		return err
	}
	args = append(args, "--cookies", file.Name(), "--flat-playlist", "--playlist-end", "60", "--dump-single-json")
	var failures []string
	for i, feed := range youtubeFeeds {
		progress(i+1, feed.Key)
		feedCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		data, runErr := youtubeJSON(feedCtx, append(append([]string{}, args...), "--", feed.URL))
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var result struct {
			Entries *[]youtubeVideo `json:"entries"`
		}
		if runErr != nil && runErr.Error() == "linn.youtube.account_error" {
			return runErr
		}
		if runErr != nil || json.Unmarshal(data, &result) != nil || result.Entries == nil {
			failures = append(failures, tr(feed.Key))
			continue
		}
		var videos []youtubeVideo
		seen := make(map[string]bool)
		for _, video := range *result.Entries {
			if youtubeID.MatchString(video.ID) && video.Title != "" && !seen[video.ID] {
				videos = append(videos, video)
				seen[video.ID] = true
			}
		}
		if feed.ID == "recommended" && len(videos) == 0 {
			failures = append(failures, tr(feed.Key))
			continue
		}
		// Publish each completed list so browsing need not wait for the slowest one.
		bridge.mu.Lock()
		if !bytes.Equal(cookies, bridge.session) {
			bridge.mu.Unlock()
			return errors.New("linn.youtube.account_error")
		}
		changed := !slices.Equal(bridge.feeds[feed.ID], videos)
		bridge.feeds[feed.ID] = videos
		bridge.updated = time.Now()
		if changed {
			bridge.revision++
			if err := bridge.saveCatalog(); err != nil {
				slog.Warn("YouTube list cache could not be saved", "error", err)
			}
		}
		bridge.mu.Unlock()
	}
	progress(len(youtubeFeeds)+1, "")
	if len(failures) > 0 {
		return fmt.Errorf("linn.youtube.feed_error: %s", strings.Join(failures, ", "))
	}
	return nil
}

func youtubeTool(name string) (string, error) {
	if executable, err := exec.LookPath(name); err == nil {
		return executable, nil
	}
	configPath, err := appConfigPath()
	if err != nil {
		return "", err
	}
	filename := name
	if runtime.GOOS == "windows" {
		filename += ".exe"
	}
	for _, directory := range []string{filepath.Join(filepath.Dir(configPath), "tools"), "/opt/homebrew/bin", "/usr/local/bin"} {
		candidate := filepath.Join(directory, filename)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("missing %s", name)
}

func youtubeArguments() ([]string, error) {
	node, err := youtubeTool("node")
	if err != nil {
		return nil, err
	}
	return []string{"--ignore-config", "--no-plugin-dirs", "--no-cache-dir", "--socket-timeout", "15", "--retries", "1", "--js-runtimes", "node:" + node}, nil
}

type youtubeOutput struct {
	bytes.Buffer
	limit int
}

func (output *youtubeOutput) Write(data []byte) (int, error) {
	if output.Len()+len(data) > output.limit {
		return 0, errors.New("YouTube response exceeds limit")
	}
	return output.Buffer.Write(data)
}

func youtubeJSON(ctx context.Context, args []string) ([]byte, error) {
	tool, err := youtubeTool("yt-dlp")
	if err != nil {
		return nil, err
	}
	runtimeDirectory, err := os.MkdirTemp("", "linn-youtube-runtime-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(runtimeDirectory)
	command := exec.CommandContext(ctx, tool, args...)
	command.Env = append(os.Environ(), "TMPDIR="+runtimeDirectory, "TMP="+runtimeDirectory, "TEMP="+runtimeDirectory)
	configureYouTubeCommand(command)
	command.WaitDelay = 3 * time.Second
	output := youtubeOutput{limit: 8 << 20}
	diagnostics := youtubeOutput{limit: 8192}
	command.Stdout, command.Stderr = &output, &diagnostics
	runErr := command.Run()
	if strings.Contains(diagnostics.String(), "YouTube account cookies are no longer valid") {
		return nil, errors.New("linn.youtube.account_error")
	}
	if err := runErr; err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Extractor output can contain signed URLs or account data; keep it out of logs.
		reason := "extractor error"
		for _, known := range []string{"Sign in to confirm", "HTTP Error 403", "HTTP Error 429", "Requested format is not available", "Video unavailable", "timed out"} {
			if strings.Contains(diagnostics.String(), known) {
				reason = known
				break
			}
		}
		return nil, fmt.Errorf("YouTube request failed (%s): %w", reason, err)
	}
	return output.Bytes(), nil
}

type youtubeFormat struct {
	URL        string `json:"url"`
	VideoCodec string `json:"vcodec"`
	AudioCodec string `json:"acodec"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
}

func youtubeVideoStream(ctx context.Context, id, sessionPath string, resolutions []int) (youtubeStream, error) {
	if !youtubeID.MatchString(id) {
		return youtubeStream{}, errors.New("invalid video ID")
	}
	args, err := youtubeArguments()
	if err != nil {
		return youtubeStream{}, err
	}
	args = append(args, "--no-playlist", "--dump-single-json", "-f", youtubeFormatSelector(resolutions), "-S", "res,vcodec:h264,acodec:m4a")
	target := []string{"--", "https://www.youtube.com/watch?v=" + id}
	// Public playback avoids account-specific SABR-only responses that expose just 360p.
	data, err := youtubeJSON(ctx, append(slices.Clone(args), target...))
	if err != nil && sessionPath != "" && ctx.Err() == nil {
		args = append(args, "--cookies", sessionPath)
		data, err = youtubeJSON(ctx, append(args, target...))
	}
	if err != nil {
		return youtubeStream{}, err
	}
	return parseYouTubeStream(data)
}

func parseYouTubeStream(data []byte) (youtubeStream, error) {
	var result struct {
		youtubeFormat
		Formats []youtubeFormat `json:"requested_formats"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return youtubeStream{}, err
	}
	if len(result.Formats) == 0 && result.URL != "" {
		result.Formats = []youtubeFormat{result.youtubeFormat}
	}
	if len(result.Formats) < 1 || len(result.Formats) > 2 {
		return youtubeStream{}, errors.New("compatible video and audio not available")
	}
	video, audio := result.Formats[0], result.Formats[len(result.Formats)-1]
	stream := youtubeStream{Width: video.Width, Height: video.Height}
	switch {
	case strings.HasPrefix(video.VideoCodec, "avc1"), video.VideoCodec == "h264":
		stream.VideoCodec = "h264"
	case strings.HasPrefix(video.VideoCodec, "hev1"), strings.HasPrefix(video.VideoCodec, "hvc1"), video.VideoCodec == "hevc":
		stream.VideoCodec = "hevc"
	case strings.HasPrefix(video.VideoCodec, "vp9"), strings.HasPrefix(video.VideoCodec, "vp09"):
		stream.VideoCodec = "vp9"
	case strings.HasPrefix(video.VideoCodec, "av01"), video.VideoCodec == "av1":
		stream.VideoCodec = "av1"
	default:
		return youtubeStream{}, errors.New("unsupported source video codec")
	}
	if strings.HasPrefix(audio.AudioCodec, "mp4a") || audio.AudioCodec == "aac" {
		stream.AudioCodec = "aac"
	}
	for _, format := range result.Formats {
		u, err := url.Parse(format.URL)
		if err != nil || u.Scheme != "https" || u.User != nil || !strings.HasSuffix(u.Hostname(), ".googlevideo.com") {
			return youtubeStream{}, errors.New("invalid media origin")
		}
		stream.URLs = append(stream.URLs, format.URL)
	}
	return stream, nil
}

const youtubeDLNAFeatures = "DLNA.ORG_OP=00;DLNA.ORG_CI=1;DLNA.ORG_FLAGS=01700000000000000000000000000000"

func (bridge *youtubeBridge) serve(writer http.ResponseWriter, request *http.Request) {
	if strings.HasSuffix(request.URL.Path, ".jpg") {
		bridge.serveThumbnail(writer, request)
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/youtube/"), ".ts")
	if !youtubeID.MatchString(id) {
		http.NotFound(writer, request)
		return
	}
	if _, ok := bridge.video(id); !ok {
		http.NotFound(writer, request)
		return
	}
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writer.Header().Set("Content-Type", "video/mp2t")
	writer.Header().Set("transferMode.dlna.org", "Streaming")
	writer.Header().Set("contentFeatures.dlna.org", youtubeDLNAFeatures)
	writer.Header().Set("Accept-Ranges", "none")
	writer.Header().Set("Cache-Control", "no-store")
	if request.Method == http.MethodHead {
		writer.WriteHeader(http.StatusOK)
		return
	}
	if r := request.Header.Get("Range"); r != "" && !strings.HasPrefix(r, "bytes=0-") {
		writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	select {
	case bridge.streams <- struct{}{}:
		defer func() { <-bridge.streams }()
	default:
		http.Error(writer, "Two videos are already playing", http.StatusServiceUnavailable)
		return
	}
	bridge.mu.Lock()
	bridge.cancelPrefetchExcept(id)
	bridge.mu.Unlock()
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	resolveCtx, resolveCancel := context.WithTimeout(ctx, 45*time.Second)
	stream, err := bridge.resolve(resolveCtx, id)
	resolveCancel()
	if err != nil {
		slog.Warn("YouTube URL resolution failed", "error", err)
		http.Error(writer, "Could not open YouTube video", http.StatusBadGateway)
		return
	}
	settings := bridge.playbackSettings()
	// shortcut: HTTP reads cannot confirm decoding; device learning needs renderer playback feedback.
	for _, codec := range settings.Codecs {
		written, streamErr := youtubeServeStream(ctx, writer, stream, codec)
		if written > 0 || ctx.Err() != nil {
			return
		}
		if streamErr == nil {
			return
		}
	}
	http.Error(writer, "YouTube stream unavailable", http.StatusBadGateway)
}

func (bridge *youtubeBridge) resolve(ctx context.Context, id string) (youtubeStream, error) {
	bridge.resolveMu.Lock()
	defer bridge.resolveMu.Unlock()
	if err := ctx.Err(); err != nil {
		return youtubeStream{}, err
	}
	bridge.mu.RLock()
	if cached, ok := bridge.resolved[id]; ok && time.Now().Before(cached.expires) {
		bridge.mu.RUnlock()
		return cached.stream, nil
	}
	session := append([]byte(nil), bridge.session...)
	resolutions := slices.Clone(bridge.playback.Resolutions)
	bridge.mu.RUnlock()
	var sessionPath string
	if len(session) != 0 {
		file, err := os.CreateTemp(bridge.root, ".youtube-session-")
		if err != nil {
			return youtubeStream{}, err
		}
		sessionPath = file.Name()
		defer os.Remove(sessionPath)
		_, err = file.Write(session)
		file.Close()
		if err != nil {
			return youtubeStream{}, err
		}
	}
	stream, err := youtubeVideoStream(ctx, id, sessionPath, resolutions)
	if err != nil {
		return youtubeStream{}, err
	}
	if err := ctx.Err(); err != nil {
		return youtubeStream{}, err
	}
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	// An account change during extraction must not repopulate the old session's cache.
	if !bytes.Equal(session, bridge.session) || !slices.Equal(resolutions, bridge.playback.Resolutions) {
		return stream, nil
	}
	for id, cached := range bridge.resolved {
		if time.Now().After(cached.expires) {
			delete(bridge.resolved, id)
		}
	}
	bridge.resolved[id] = youtubeResolved{stream: stream, expires: time.Now().Add(5 * time.Minute)}
	return stream, nil
}

// Caller holds mu. Playback takes priority over unrelated speculative lookups.
func (bridge *youtubeBridge) cancelPrefetchExcept(keep string) {
	for id, job := range bridge.prefetch {
		if id != keep {
			job.cancel()
			delete(bridge.prefetch, id)
		}
	}
}

func (bridge *youtubeBridge) prewarm(entries []mediaEntry) {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if bridge.ctx.Err() != nil || len(bridge.streams) > 0 {
		return
	}
	var ids []string
	for _, entry := range entries {
		id := filepath.Base(entry.Rel)
		if !entry.Dir && bridge.owns(entry.Rel) && youtubeID.MatchString(id) && !slices.Contains(ids, id) {
			ids = append(ids, id)
			if len(ids) == 3 {
				break
			}
		}
	}
	if len(ids) == 0 {
		return
	}
	for id, job := range bridge.prefetch {
		if !slices.Contains(ids, id) || time.Now().After(job.expires) {
			job.cancel()
			delete(bridge.prefetch, id)
		}
	}
	type lookup struct {
		id     string
		ctx    context.Context
		cancel context.CancelFunc
	}
	var pending []lookup
	for _, id := range ids {
		if cached, ok := bridge.resolved[id]; ok && time.Now().Before(cached.expires) {
			continue
		}
		if bridge.prefetch[id] != nil {
			continue
		}
		ctx, cancel := context.WithCancel(bridge.ctx)
		// Keep failed attempts briefly to avoid hammering YouTube on repeated TV probes.
		bridge.prefetch[id] = &youtubePrefetch{cancel: cancel, expires: time.Now().Add(2 * time.Minute)}
		pending = append(pending, lookup{id, ctx, cancel})
	}
	if len(pending) == 0 {
		return
	}
	go func() {
		for _, job := range pending {
			ctx, cancel := context.WithTimeout(job.ctx, 45*time.Second)
			_, _ = bridge.resolve(ctx, job.id)
			cancel()
			job.cancel()
		}
	}()
}

func (bridge *youtubeBridge) serveThumbnail(writer http.ResponseWriter, request *http.Request) {
	id := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/youtube/"), ".jpg")
	if !youtubeID.MatchString(id) {
		http.NotFound(writer, request)
		return
	}
	if _, ok := bridge.video(id); !ok {
		http.NotFound(writer, request)
		return
	}
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
	defer cancel()
	upstream, err := http.NewRequestWithContext(ctx, request.Method, "https://i.ytimg.com/vi/"+id+"/hqdefault.jpg", nil)
	if err != nil {
		http.Error(writer, "Thumbnail unavailable", http.StatusBadGateway)
		return
	}
	response, err := http.DefaultClient.Do(upstream)
	if err != nil {
		http.Error(writer, "Thumbnail unavailable", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		http.NotFound(writer, request)
		return
	}
	writer.Header().Set("Content-Type", "image/jpeg")
	writer.Header().Set("Cache-Control", "private, max-age=3600")
	if request.Method == http.MethodGet {
		_, _ = io.Copy(writer, io.LimitReader(response.Body, 2<<20))
	}
}
