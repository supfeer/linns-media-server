package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

var searchToken = regexp.MustCompile(`^(?:"(?:[^"\\]|\\["\\])*"|[a-zA-Z_@][a-zA-Z0-9_:@.-]*|!=|<=|>=|[()=<>*])`)

type dlnaSearch struct {
	property, operator, value string
	left, right               *dlnaSearch
}

func parseDLNASearch(criteria string) (*dlnaSearch, error) {
	if len(criteria) > 2048 {
		return nil, errors.New("search too long")
	}
	var tokens []string
	for input := strings.TrimSpace(criteria); input != ""; input = strings.TrimSpace(input) {
		token := searchToken.FindString(input)
		if token == "" || len(tokens) >= 128 {
			return nil, errors.New("invalid search syntax")
		}
		tokens = append(tokens, token)
		input = input[len(token):]
	}
	if len(tokens) == 1 && tokens[0] == "*" {
		return &dlnaSearch{operator: "*"}, nil
	}
	pos := 0
	var expression func(int) (*dlnaSearch, error)
	expression = func(level int) (*dlnaSearch, error) {
		if level < 2 {
			left, err := expression(level + 1)
			if err != nil {
				return nil, err
			}
			operator := []string{"or", "and"}[level]
			for pos < len(tokens) && strings.EqualFold(tokens[pos], operator) {
				pos++
				right, err := expression(level + 1)
				if err != nil {
					return nil, err
				}
				left = &dlnaSearch{operator: operator, left: left, right: right}
			}
			return left, nil
		}
		if pos < len(tokens) && tokens[pos] == "(" {
			pos++
			node, err := expression(0)
			if err != nil || pos >= len(tokens) || tokens[pos] != ")" {
				return nil, errors.New("unbalanced search")
			}
			pos++
			return node, nil
		}
		if pos+3 > len(tokens) {
			return nil, errors.New("incomplete search")
		}
		node := &dlnaSearch{property: tokens[pos], operator: strings.ToLower(tokens[pos+1])}
		value := tokens[pos+2]
		pos += 3
		if !slices.Contains([]string{"dc:title", "upnp:class", "@refID"}, node.property) {
			return nil, errors.New("unsupported search property")
		}
		if node.operator == "exists" {
			if value != "true" && value != "false" {
				return nil, errors.New("invalid existence test")
			}
			node.value = value
		} else {
			if !slices.Contains([]string{"=", "!=", "<", "<=", ">", ">=", "contains", "doesnotcontain", "derivedfrom", "startswith"}, node.operator) {
				return nil, errors.New("unsupported search operator")
			}
			decoded, err := strconv.Unquote(value)
			if err != nil {
				return nil, errors.New("invalid search value")
			}
			node.value = decoded
		}
		return node, nil
	}
	node, err := expression(0)
	if err != nil || pos != len(tokens) {
		return nil, errors.New("unsupported search criteria")
	}
	return node, nil
}

func (node *dlnaSearch) matches(entry mediaEntry) bool {
	switch node.operator {
	case "*":
		return true
	case "and":
		return node.left.matches(entry) && node.right.matches(entry)
	case "or":
		return node.left.matches(entry) || node.right.matches(entry)
	}
	if node.operator == "exists" {
		return (node.property != "@refID") == (node.value == "true")
	}
	if node.property == "@refID" {
		return false
	}
	actual := entry.Name
	if node.property == "upnp:class" {
		actual = "object.item.videoItem"
		if entry.Dir {
			actual = "object.container.storageFolder"
		}
	}
	actual, value := strings.ToLower(actual), strings.ToLower(node.value)
	comparison := strings.Compare(actual, value)
	if a, err := strconv.ParseInt(actual, 10, 64); err == nil {
		if b, err := strconv.ParseInt(value, 10, 64); err == nil {
			comparison = 0
			if a < b {
				comparison = -1
			} else if a > b {
				comparison = 1
			}
		}
	}
	switch node.operator {
	case "=":
		return comparison == 0
	case "!=":
		return comparison != 0
	case "<":
		return comparison < 0
	case "<=":
		return comparison <= 0
	case ">":
		return comparison > 0
	case ">=":
		return comparison >= 0
	case "contains":
		return strings.Contains(actual, value)
	case "doesnotcontain":
		return !strings.Contains(actual, value)
	case "startswith":
		return strings.HasPrefix(actual, value)
	case "derivedfrom":
		return actual == value || strings.HasPrefix(actual, value+".")
	}
	return false
}

func (node *dlnaSearch) queries() []string {
	if node.left != nil {
		result := node.left.queries()
		for _, q := range node.right.queries() {
			if !slices.Contains(result, q) {
				result = append(result, q)
			}
		}
		return result
	}
	if node.property == "dc:title" && slices.Contains([]string{"contains", "=", "startswith"}, node.operator) && strings.TrimSpace(node.value) != "" {
		return []string{strings.TrimSpace(node.value)}
	}
	return nil
}

func (server *mediaServer) handleSearch(writer http.ResponseWriter, request *http.Request) {
	bridge := server.youtube
	if bridge == nil {
		writeUPnPError(writer, 708, "Unsupported or invalid search criteria")
		return
	}
	client, _, _ := net.SplitHostPort(request.RemoteAddr)
	bridge.setSearchStatus("linn.youtube.search_running", client, -1)
	fail := func(code int, description string) {
		server.trace.record(request, "Search", map[string]string{"upnp_error": strconv.Itoa(code)})
		bridge.setSearchStatus("linn.youtube.search_failed", client, code)
		writeUPnPError(writer, code, description)
	}
	values, err := decodeSOAPValues(request.Body, map[string]bool{"ContainerID": true, "SearchCriteria": true, "Filter": true, "StartingIndex": true, "RequestedCount": true, "SortCriteria": true})
	server.trace.record(request, "Search", values)
	if err != nil || len(values) != 6 {
		fail(402, "Invalid Args")
		return
	}
	rel, err := relForID(values["ContainerID"])
	if err != nil {
		fail(710, "No Such Container")
		return
	}
	var container mediaEntry
	if bridge.owns(rel) {
		entries, ok := bridge.entries(rel, true)
		if !ok || len(entries) != 1 {
			fail(710, "No Such Container")
			return
		}
		container = entries[0]
	} else {
		var ok bool
		container, ok = server.library.entry(rel)
		if !ok {
			fail(710, "No Such Container")
			return
		}
	}
	if !container.Dir {
		fail(710, "No Such Container")
		return
	}
	criteria, err := parseDLNASearch(values["SearchCriteria"])
	if err != nil {
		fail(708, "Unsupported or invalid search criteria")
		return
	}
	start, err := nonnegativeInt(values["StartingIndex"])
	if err != nil {
		fail(402, "Invalid Args")
		return
	}
	count, err := nonnegativeInt(values["RequestedCount"])
	if err != nil {
		fail(402, "Invalid Args")
		return
	}
	sortOrder := values["SortCriteria"]
	if sortOrder != "" && sortOrder != "+dc:title" && sortOrder != "-dc:title" {
		fail(709, "Unsupported or invalid sort criteria")
		return
	}
	var candidates []mediaEntry
	queries := criteria.queries()
	// shortcut: expand at most four title terms and 30 YouTube hits per term; add provider paging when needed.
	if len(queries) > 4 {
		fail(708, "Unsupported or invalid search criteria")
		return
	}
	if rel == "." || rel == bridge.source().ID {
		ctx, cancel := context.WithTimeout(request.Context(), 25*time.Second)
		defer cancel()
		for _, query := range queries {
			entries, err := bridge.search(ctx, query)
			if err != nil {
				if errors.Is(err, errInvalidYouTubeSearch) {
					fail(708, "Unsupported or invalid search criteria")
					return
				}
				fail(720, "Cannot process the request")
				return
			}
			candidates = append(candidates, entries...)
		}
	}
	var local []mediaEntry
	server.library.mu.RLock()
	for path, entry := range server.library.entries {
		if path != rel && (rel == "." || strings.HasPrefix(path, rel+"/")) {
			local = append(local, entry)
		}
	}
	server.library.mu.RUnlock()
	slices.SortFunc(local, func(a, b mediaEntry) int { return strings.Compare(a.Rel, b.Rel) })
	candidates = append(candidates, local...)
	if rel == "." || rel == bridge.source().ID {
		folders, _ := bridge.entries(bridge.source().ID, false)
		candidates = append(candidates, folders...)
	}
	for _, feed := range youtubeFeeds {
		parent := bridge.source().ID + "/" + feed.ID
		if rel == "." || rel == bridge.source().ID || rel == parent {
			entries, _ := bridge.entries(parent, false)
			candidates = append(candidates, entries...)
		}
	}
	if rel == "." || rel == bridge.source().ID || strings.HasPrefix(rel, bridge.source().ID+"/search/") || rel == bridge.source().ID+"/search" {
		var cached []mediaEntry
		bridge.mu.RLock()
		for key, result := range bridge.searches {
			for _, entry := range bridge.searchResultEntries(key, result) {
				if rel == "." || strings.HasPrefix(entry.Rel, rel+"/") {
					cached = append(cached, entry)
				}
			}
		}
		bridge.mu.RUnlock()
		slices.SortFunc(cached, func(a, b mediaEntry) int { return strings.Compare(a.Rel, b.Rel) })
		candidates = append(candidates, cached...)
	}
	var entries []mediaEntry
	seen := make(map[string]bool)
	for _, entry := range candidates {
		key := entry.Rel
		if bridge.owns(entry.Rel) && !entry.Dir {
			key = entry.Rel[strings.LastIndex(entry.Rel, "/")+1:]
		}
		if !seen[key] && criteria.matches(entry) {
			entries = append(entries, entry)
			seen[key] = true
		}
	}
	if sortOrder != "" {
		slices.SortStableFunc(entries, func(a, b mediaEntry) int {
			comparison := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
			if sortOrder[0] == '-' {
				return -comparison
			}
			return comparison
		})
	}
	total := len(entries)
	if start > total {
		start = total
	}
	entries = entries[start:]
	if count != 0 && count < len(entries) {
		entries = entries[:count]
	}
	didl, err := server.marshalDIDL(entries)
	if err != nil {
		fail(501, "Action Failed")
		return
	}
	writeSOAPResponse(writer, contentDirectoryURN, "Search", [][2]string{{"Result", string(didl)}, {"NumberReturned", strconv.Itoa(len(entries))}, {"TotalMatches", strconv.Itoa(total)}, {"UpdateID", fmt.Sprint(server.library.systemUpdateID())}})
	bridge.setSearchStatus("linn.youtube.search_complete", client, len(entries))
	server.trace.record(request, "Search", map[string]string{"returned": strconv.Itoa(len(entries)), "total": strconv.Itoa(total)})
	bridge.prewarm(entries)
}
