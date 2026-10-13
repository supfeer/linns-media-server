package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"time"
)

type youtubePlaybackSettings struct {
	Codecs      []string `json:"codecs"`
	Resolutions []int    `json:"resolutions"`
}

func (settings youtubePlaybackSettings) normalized() (youtubePlaybackSettings, error) {
	codecs := []string{"hevc", "h264"}
	resolutions := []int{2160, 1440, 1080, 720, 480}
	if len(settings.Codecs) == 0 {
		settings.Codecs = codecs
	}
	if len(settings.Resolutions) == 0 {
		settings.Resolutions = resolutions
	}
	if len(settings.Codecs) != len(codecs) || len(settings.Resolutions) != len(resolutions) {
		return settings, errors.New("invalid YouTube playback order")
	}
	for _, codec := range codecs {
		if !slices.Contains(settings.Codecs, codec) {
			return settings, errors.New("invalid YouTube codec order")
		}
	}
	for _, resolution := range resolutions {
		if !slices.Contains(settings.Resolutions, resolution) {
			return settings, errors.New("invalid YouTube resolution order")
		}
	}
	settings.Codecs = slices.Clone(settings.Codecs)
	settings.Resolutions = slices.Clone(settings.Resolutions)
	return settings, nil
}

func (bridge *youtubeBridge) playbackSettings() youtubePlaybackSettings {
	bridge.mu.RLock()
	defer bridge.mu.RUnlock()
	return youtubePlaybackSettings{slices.Clone(bridge.playback.Codecs), slices.Clone(bridge.playback.Resolutions)}
}

func (bridge *youtubeBridge) setPlaybackSettings(settings youtubePlaybackSettings) error {
	settings, err := settings.normalized()
	if err != nil {
		return err
	}
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if slices.Equal(settings.Codecs, bridge.playback.Codecs) && slices.Equal(settings.Resolutions, bridge.playback.Resolutions) {
		return nil
	}
	bridge.cancelPrefetchExcept("")
	bridge.playback = settings
	bridge.resolved = make(map[string]youtubeResolved)
	return nil
}

func youtubeFormatSelector(resolutions []int) string {
	var formats []string
	for _, height := range resolutions {
		filter := fmt.Sprintf("[height<=%d]", height)
		for _, lower := range []int{1440, 1080, 720, 480} {
			if lower < height {
				filter += fmt.Sprintf("[height>%d]", lower)
				break
			}
		}
		formats = append(formats, "bv"+filter+"[dynamic_range=SDR]+ba[ext=m4a]", "b"+filter+"[ext=mp4]")
	}
	return strings.Join(formats, "/")
}

type youtubeStream struct {
	URLs       []string
	VideoCodec string
	AudioCodec string
	Width      int
	Height     int
}

func youtubeStreamArguments(stream youtubeStream, codec, platform string) []string {
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error"}
	for _, url := range stream.URLs {
		args = append(args, "-rw_timeout", "15000000", "-i", url)
	}
	args = append(args, "-map", "0:v:0", "-map", fmt.Sprintf("%d:a:0", len(stream.URLs)-1))
	if codec == stream.VideoCodec {
		args = append(args, "-c:v", "copy")
	} else if platform == "darwin" {
		bitrate := "8M"
		if stream.Height > 1080 {
			bitrate = "18M"
		}
		args = append(args, "-c:v", codec+"_videotoolbox", "-b:v", bitrate, "-pix_fmt", "yuv420p", "-g", "60", "-bf", "0", "-realtime", "1")
	} else {
		encoder := "libx264"
		if codec == "hevc" {
			encoder = "libx265"
		}
		args = append(args, "-c:v", encoder, "-preset", "veryfast", "-crf", "23", "-pix_fmt", "yuv420p", "-g", "60", "-bf", "0")
	}
	if stream.AudioCodec == "aac" {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, "-c:a", "aac", "-b:a", "192k", "-ac", "2")
	}
	return append(args, "-bsf:v", codec+"_mp4toannexb", "-mpegts_flags", "+resend_headers", "-muxdelay", "0", "-f", "mpegts", "pipe:1")
}

func youtubeServeStream(ctx context.Context, writer http.ResponseWriter, stream youtubeStream, codec string) (int64, error) {
	executable, err := youtubeTool("ffmpeg")
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(ctx, executable, youtubeStreamArguments(stream, codec, runtime.GOOS)...)
	configureYouTubeCommand(command)
	command.WaitDelay = 3 * time.Second
	pipe, err := command.StdoutPipe()
	if err != nil {
		return 0, err
	}
	command.Stderr = io.Discard
	if err = command.Start(); err != nil {
		return 0, err
	}
	defer func() { cancel(); pipe.Close(); command.Wait() }()
	buffer := make([]byte, 32*1024)
	var written int64
	for {
		n, readErr := pipe.Read(buffer)
		if n > 0 {
			count, err := writer.Write(buffer[:n])
			written += int64(count)
			if err != nil {
				return written, err
			}
			if flusher, ok := writer.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr == io.EOF && written > 0 {
				return written, nil
			}
			return written, readErr
		}
	}
}
