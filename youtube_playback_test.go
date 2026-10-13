package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
)

const testYouTubeStreamJSON = `{"requested_formats":[{"url":"https://r.googlevideo.com/video","vcodec":"vp09.00.51.08","width":3840,"height":2160},{"url":"https://r.googlevideo.com/audio","acodec":"mp4a.40.2"}]}`

func mockYouTubeTools(t *testing.T, scripts map[string]string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixtures")
	}
	directory := t.TempDir()
	scripts["node"] = "exit 0\n"
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("#!/bin/sh\n"+script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", directory)
	return directory
}

func TestYouTubePlaybackPreferences(t *testing.T) {
	bridge := testYouTubeBridge(t)
	settings := bridge.playbackSettings()
	if settings.Codecs[0] != "hevc" || settings.Resolutions[0] != 2160 {
		t.Fatal("4K HEVC is not preferred")
	}
	settings.Codecs[0], settings.Codecs[1] = settings.Codecs[1], settings.Codecs[0]
	settings.Resolutions[0], settings.Resolutions[2] = settings.Resolutions[2], settings.Resolutions[0]
	bridge.resolved["jNQXAC9IVRw"] = youtubeResolved{expires: time.Now().Add(time.Hour)}
	if err := bridge.setPlaybackSettings(settings); err != nil {
		t.Fatal(err)
	}
	if len(bridge.resolved) != 0 {
		t.Fatal("old quality stayed cached")
	}
	data, err := json.Marshal(appConfig{YouTubePlayback: settings})
	if err != nil {
		t.Fatal(err)
	}
	var config appConfig
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(config.YouTubePlayback.Resolutions, settings.Resolutions) || !slices.Equal(config.YouTubePlayback.Codecs, settings.Codecs) {
		t.Fatal("settings did not round trip")
	}
	settings.Resolutions[0] = 720
	if bridge.playbackSettings().Resolutions[0] != 1080 {
		t.Fatal("settings alias caller memory")
	}
	if bridge.setPlaybackSettings(settings) == nil {
		t.Fatal("accepted duplicate resolution")
	}
	settings = bridge.playbackSettings()
	settings.Codecs[0] = "untrusted"
	if bridge.setPlaybackSettings(settings) == nil {
		t.Fatal("accepted unknown codec")
	}
	selector := youtubeFormatSelector([]int{1080, 2160, 1440, 720, 480})
	if !strings.HasPrefix(selector, "bv[height<=1080][height>720]") || strings.Index(selector, "height<=2160") > strings.Index(selector, "height<=1440") || !strings.HasSuffix(selector, "b[height<=480][ext=mp4]") {
		t.Fatalf("wrong priority: %s", selector)
	}
}

func TestYouTubeOrderPickerSwapsAndRollsBack(t *testing.T) {
	app := test.NewApp()
	defer app.Quit()
	var saved []string
	fail := false
	row := newYouTubeOrderPicker([]string{"2160p", "1080p", "720p"}, []string{"2160p", "1080p", "720p"}, func(order []string) error {
		if fail {
			return errors.New("disk full")
		}
		saved = slices.Clone(order)
		return nil
	})
	var pickers []*widget.Select
	for _, object := range row.Objects {
		pickers = append(pickers, object.(*fyne.Container).Objects[0].(*widget.Select))
	}
	pickers[0].SetSelected("720p")
	if !slices.Equal(saved, []string{"720p", "1080p", "2160p"}) || pickers[2].Selected != "2160p" {
		t.Fatal("priority was not swapped")
	}
	fail = true
	pickers[1].SetSelected("720p")
	if pickers[0].Selected != "720p" || pickers[1].Selected != "1080p" {
		t.Fatal("failed save changed the visible order")
	}
}

func TestYouTubeStreamCodecConversion(t *testing.T) {
	stream, err := parseYouTubeStream([]byte(testYouTubeStreamJSON))
	if err != nil || stream.VideoCodec != "vp9" || stream.AudioCodec != "aac" || stream.Width != 3840 || stream.Height != 2160 {
		t.Fatalf("parse: %+v %v", stream, err)
	}
	args := strings.Join(youtubeStreamArguments(stream, "hevc", "darwin"), " ")
	for _, want := range []string{"-map 0:v:0 -map 1:a:0", "-c:v hevc_videotoolbox", "-b:v 18M", "-c:a copy", "hevc_mp4toannexb"} {
		if !strings.Contains(args, want) {
			t.Fatalf("missing %q in %s", want, args)
		}
	}
	stream.VideoCodec = "h264"
	if !strings.Contains(strings.Join(youtubeStreamArguments(stream, "h264", "darwin"), " "), "-c:v copy") {
		t.Fatal("unneeded transcode")
	}
}

func TestYouTubePlaybackUsesPublicExtractionBeforeAccount(t *testing.T) {
	for _, restricted := range []bool{false, true} {
		t.Run(map[bool]string{false: "public", true: "restricted"}[restricted], func(t *testing.T) {
			script := `case " $* " in *" --cookies "*) printf 'account\n' >> "$CALLS";; *) printf 'public\n' >> "$CALLS"; `
			if restricted {
				script += "exit 1; "
			}
			script += "esac\nprintf '%s' '" + testYouTubeStreamJSON + "'\n"
			directory := mockYouTubeTools(t, map[string]string{"yt-dlp": script})
			marker := filepath.Join(directory, "calls")
			t.Setenv("CALLS", marker)
			settings, _ := (youtubePlaybackSettings{}).normalized()
			_, err := youtubeVideoStream(t.Context(), "jNQXAC9IVRw", "test-session", settings.Resolutions)
			if err != nil {
				t.Fatal(err)
			}
			calls, _ := os.ReadFile(marker)
			want := "public\n"
			if restricted {
				want += "account\n"
			}
			if string(calls) != want {
				t.Fatalf("attempt order %q", calls)
			}
		})
	}
}

func TestYouTubePrefetchBoundedOrderedAndReusable(t *testing.T) {
	script := "for arg; do last=$arg; done\nprintf '%s\\n' \"$last\" >> \"$CALLS\"\nprintf '%s' '" + testYouTubeStreamJSON + "'\n"
	directory := mockYouTubeTools(t, map[string]string{"yt-dlp": script})
	marker := filepath.Join(directory, "calls")
	t.Setenv("CALLS", marker)
	bridge := testYouTubeBridge(t)
	ctx, cancel := context.WithCancel(t.Context())
	bridge.ctx = ctx
	t.Cleanup(func() { cancel(); bridge.resolveMu.Lock(); bridge.resolveMu.Unlock() })
	bridge.feeds["recommended"] = []youtubeVideo{{ID: "aaaaaaaaaaa"}, {ID: "bbbbbbbbbbb"}, {ID: "ccccccccccc"}, {ID: "ddddddddddd"}}
	entries, _ := bridge.entries(bridge.source().ID+"/recommended", false)
	bridge.prewarm(entries)
	deadline := time.After(3 * time.Second)
	for {
		bridge.mu.RLock()
		count := len(bridge.resolved)
		bridge.mu.RUnlock()
		if count == 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("prewarm did not complete")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	bridge.prewarm(entries)
	if _, err := bridge.resolve(ctx, "bbbbbbbbbbb"); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(marker)
	want := "https://www.youtube.com/watch?v=aaaaaaaaaaa\nhttps://www.youtube.com/watch?v=bbbbbbbbbbb\nhttps://www.youtube.com/watch?v=ccccccccccc\n"
	if string(calls) != want {
		t.Fatalf("unbounded, repeated or unordered prefetch: %q", calls)
	}
	files, err := os.ReadDir(bridge.root)
	if err != nil || len(files) != 0 {
		t.Fatal("prefetch wrote media")
	}
}

func TestYouTubeCodecFallbackBeforeFirstByteOnly(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "no output", true: "partial output"}[partial], func(t *testing.T) {
			script := `case " $* " in *hevc_mp4toannexb*) printf 'hevc\n' >> "$CALLS"; `
			if partial {
				script += "printf FIRST; "
			}
			script += "exit 1;; *) printf 'h264\\n' >> \"$CALLS\"; printf SECOND;; esac\n"
			directory := mockYouTubeTools(t, map[string]string{"ffmpeg": script})
			marker := filepath.Join(directory, "calls")
			t.Setenv("CALLS", marker)
			bridge := testYouTubeBridge(t)
			bridge.resolved["jNQXAC9IVRw"] = youtubeResolved{stream: youtubeStream{URLs: []string{"https://r.googlevideo.com/video"}, VideoCodec: "vp9"}, expires: time.Now().Add(time.Minute)}
			response := httptest.NewRecorder()
			bridge.serve(response, httptest.NewRequest(http.MethodGet, "/youtube/jNQXAC9IVRw.ts", nil))
			calls, _ := os.ReadFile(marker)
			wantCalls, wantBody := "hevc\nh264\n", "SECOND"
			if partial {
				wantCalls, wantBody = "hevc\n", "FIRST"
			}
			if response.Code != 200 || string(calls) != wantCalls || response.Body.String() != wantBody {
				t.Fatalf("fallback corrupted stream: %d %q %q", response.Code, calls, response.Body.String())
			}
		})
	}
}

func TestYouTube4KStreamingIntegration(t *testing.T) {
	if os.Getenv("LINN_YOUTUBE_4K_TEST") != "1" {
		t.Skip("set LINN_YOUTUBE_4K_TEST=1")
	}
	bridge := testYouTubeBridge(t)
	bridge.feeds["recommended"] = []youtubeVideo{{ID: "aqz-KE-bpKQ", Title: "Big Buck Bunny"}}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	started := time.Now()
	source, err := bridge.resolve(ctx, "aqz-KE-bpKQ")
	if err != nil {
		t.Fatal(err)
	}
	if source.Width != 3840 || source.Height != 2160 {
		t.Fatalf("source is %dx%d", source.Width, source.Height)
	}
	t.Logf("Resolved 4K %s source in %s", source.VideoCodec, time.Since(started))
	server := httptest.NewServer(http.HandlerFunc(bridge.serve))
	defer server.Close()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/youtube/aqz-KE-bpKQ.ts", nil)
	started = time.Now()
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	packet := make([]byte, 188)
	if response.StatusCode != 200 {
		t.Fatalf("stream status %d", response.StatusCode)
	}
	if _, err := io.ReadFull(response.Body, packet); err != nil || packet[0] != 0x47 {
		t.Fatalf("not MPEG-TS: %v", err)
	}
	t.Logf("Warm 4K first packet: %s", time.Since(started))
	ffprobe, err := youtubeTool("ffprobe")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, ffprobe, "-v", "error", "-read_intervals", "%+2", "-show_entries", "stream=codec_name,width,height", "-of", "json", "-i", "pipe:0")
	command.Stdin = io.MultiReader(strings.NewReader(string(packet)), response.Body)
	output, err := command.Output()
	cancel()
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Streams []struct {
			Codec         string `json:"codec_name"`
			Width, Height int
		}
	}
	if err := json.Unmarshal(output, &probe); err != nil {
		t.Fatal(err)
	}
	video, audio := false, false
	for _, stream := range probe.Streams {
		video = video || stream.Codec == "hevc" && stream.Width == 3840 && stream.Height == 2160
		audio = audio || stream.Codec == "aac"
	}
	if !video || !audio {
		t.Fatalf("unexpected stream: %s", output)
	}
	t.Log("Verified HEVC 3840×2160 with AAC audio, no media file")
}

func TestYouTubePlaybackCancelsUnrelatedPrefetch(t *testing.T) {
	// Signal only after the child exists, so cancellation tests the complete process group.
	script := `case "$*" in *aaaaaaaaaaa*) /bin/sleep 10 & printf x > "$STARTED"; wait;; esac
printf '%s' '` + testYouTubeStreamJSON + `'
`
	directory := mockYouTubeTools(t, map[string]string{"yt-dlp": script, "ffmpeg": "printf MEDIA\n"})
	marker := filepath.Join(directory, "started")
	t.Setenv("STARTED", marker)
	bridge := testYouTubeBridge(t)
	bridge.session = []byte("test-session")
	bridge.feeds["later"] = []youtubeVideo{{ID: "aaaaaaaaaaa"}, {ID: "bbbbbbbbbbb"}, {ID: "ccccccccccc"}}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	bridge.ctx = ctx
	t.Cleanup(func() { cancel(); bridge.resolveMu.Lock(); bridge.resolveMu.Unlock() })
	entries, _ := bridge.entries(bridge.source().ID+"/later", false)
	bridge.prewarm(entries)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("prefetch not started")
		}
		time.Sleep(time.Millisecond)
	}
	response := httptest.NewRecorder()
	bridge.serve(response, httptest.NewRequest(http.MethodGet, "/youtube/jNQXAC9IVRw.ts", nil).WithContext(ctx))
	if response.Code != 200 || response.Body.String() != "MEDIA" || ctx.Err() != nil {
		t.Fatalf("playback blocked on speculative lookup: %d %q", response.Code, response.Body.String())
	}
	bridge.resolveMu.Lock()
	bridge.resolveMu.Unlock()
	files, err := os.ReadDir(bridge.root)
	if err != nil || len(files) != 0 {
		t.Fatalf("canceled lookup left temporary files: %v %v", files, err)
	}
}
