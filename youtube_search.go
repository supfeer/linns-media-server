package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type youtubeSearchResult struct {
	Query   string
	Videos  []youtubeVideo
	Updated time.Time
}

var errInvalidYouTubeSearch = errors.New("invalid search text")

func youtubeSearchKey(query string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(query))) }

func (bridge *youtubeBridge) search(ctx context.Context, query string) ([]mediaEntry, error) {
	query = strings.TrimSpace(query)
	if query == "" || !utf8.ValidString(query) || utf8.RuneCountInString(query) > 200 || strings.IndexFunc(query, unicode.IsControl) >= 0 {
		return nil, errInvalidYouTubeSearch
	}
	select {
	case bridge.searchSlot <- struct{}{}:
		defer func() { <-bridge.searchSlot }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := youtubeSearchKey(query)
	bridge.mu.RLock()
	cached, ok := bridge.searches[key]
	bridge.mu.RUnlock()
	if ok && time.Since(cached.Updated) < 5*time.Minute {
		return bridge.searchResultEntries(key, cached), nil
	}
	args, err := youtubeArguments()
	if err != nil {
		return nil, err
	}
	args = append(args, "--flat-playlist", "--playlist-end", "30", "--dump-single-json", "--", "ytsearch30:"+query)
	data, err := youtubeJSON(ctx, args)
	if err != nil {
		return nil, err
	}
	var result struct {
		Entries *[]youtubeVideo `json:"entries"`
	}
	if err = json.Unmarshal(data, &result); err != nil || result.Entries == nil {
		return nil, errors.New("invalid search response")
	}
	var videos []youtubeVideo
	seen := make(map[string]bool)
	for _, video := range *result.Entries {
		if youtubeID.MatchString(video.ID) && video.Title != "" && !seen[video.ID] {
			seen[video.ID] = true
			videos = append(videos, video)
			if len(videos) == 30 {
				break
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cached = youtubeSearchResult{Query: query, Videos: videos, Updated: time.Now()}
	bridge.mu.Lock()
	// Keep only a small in-memory catalogue; search never writes videos or query history to disk.
	if _, exists := bridge.searches[key]; !exists && len(bridge.searches) >= 8 {
		oldest := ""
		for candidate, result := range bridge.searches {
			if oldest == "" || result.Updated.Before(bridge.searches[oldest].Updated) {
				oldest = candidate
			}
		}
		delete(bridge.searches, oldest)
	}
	bridge.searches[key] = cached
	bridge.mu.Unlock()
	return bridge.searchResultEntries(key, cached), nil
}

func (bridge *youtubeBridge) searchResultEntries(key string, result youtubeSearchResult) []mediaEntry {
	entries := make([]mediaEntry, 0, len(result.Videos))
	for _, video := range result.Videos {
		entries = append(entries, mediaEntry{Rel: bridge.source().ID + "/search/" + key + "/" + video.ID, Name: video.Title})
	}
	return entries
}

// Caller holds mu. Search-result IDs remain browsable and playable after the Search response.
func (bridge *youtubeBridge) searchEntries(rel string, metadata bool) ([]mediaEntry, bool) {
	root := bridge.source().ID + "/search"
	if rel == root {
		if metadata {
			return []mediaEntry{{Rel: root, Name: tr("linn.youtube.search_results"), Dir: true}}, true
		}
		var entries []mediaEntry
		for key, result := range bridge.searches {
			entries = append(entries, mediaEntry{Rel: root + "/" + key, Name: result.Query, Dir: true})
		}
		slices.SortFunc(entries, func(a, b mediaEntry) int { return strings.Compare(a.Rel, b.Rel) })
		return entries, true
	}
	for key, result := range bridge.searches {
		parent := root + "/" + key
		if rel == parent {
			if metadata {
				return []mediaEntry{{Rel: parent, Name: result.Query, Dir: true}}, true
			}
			return bridge.searchResultEntries(key, result), true
		}
		if metadata && strings.HasPrefix(rel, parent+"/") {
			id := strings.TrimPrefix(rel, parent+"/")
			for _, video := range result.Videos {
				if video.ID == id {
					return []mediaEntry{{Rel: rel, Name: video.Title}}, true
				}
			}
		}
	}
	return nil, false
}

func (bridge *youtubeBridge) setSearchStatus(key, client string, count int) {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if count < 0 {
		bridge.searchStatus = fmt.Sprintf(tr(key), client)
	} else {
		bridge.searchStatus = fmt.Sprintf(tr(key), client, count)
	}
}

func (bridge *youtubeBridge) currentSearchStatus() string {
	bridge.mu.RLock()
	defer bridge.mu.RUnlock()
	if bridge.searchStatus == "" {
		return tr("linn.youtube.search_waiting")
	}
	return bridge.searchStatus
}
