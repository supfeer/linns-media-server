package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDLNASearchCriteria(t *testing.T) {
	for _, check := range []struct {
		criteria, title string
		dir, want       bool
	}{
		{`dc:title contains "КОТ"`, `Кот Борис`, false, true},
		{`(dc:title contains "кот") and (upnp:class derivedfrom "object.item.videoItem")`, `Кот Борис`, false, true},
		{`dc:title contains "кот" and upnp:class derivedfrom "object.item"`, `Кот Борис`, true, false},
		{`dc:title contains "one" or dc:title contains "two" and upnp:class = "object.item.audioItem"`, `one`, false, true},
		{`(dc:title contains "one" or dc:title contains "two") and upnp:class = "object.item.audioItem"`, `one`, false, false},
		{`@refID exists false and upnp:class derivedfrom "object.item"`, `Anything`, false, true},
		{`dc:title exists true`, `Anything`, false, true},
		{`dc:title contains "say \"hello\""`, `Say "hello" to the cat`, false, true},
		{`dc:title doesNotContain "dog"`, `Cat`, false, true},
		{`dc:title startsWith "cat"`, `Cat video`, false, true},
		{`dc:title > "9"`, `10`, false, true},
		{`upnp:class derivedfrom "object.item.video"`, `Anything`, false, false},
		{`*`, `Anything`, true, true},
	} {
		t.Run(check.criteria, func(t *testing.T) {
			node, err := parseDLNASearch(check.criteria)
			if err != nil {
				t.Fatal(err)
			}
			if got := node.matches(mediaEntry{Name: check.title, Dir: check.dir}); got != check.want {
				t.Fatalf("got %v want %v", got, check.want)
			}
		})
	}
	for _, criteria := range []string{"", `dc:title contains`, `dc:title contains "cat" garbage`, `dc:title contains "cat" or`, `(dc:title contains "cat"`, `dc:title like "cat"`, `unknown contains "cat"`, `dc:title exists "true"`, `dc:title = 'cat'`, `dc:title contains "bad\nquote"`, strings.Repeat("(", 200) + `dc:title contains "x"` + strings.Repeat(")", 200)} {
		if _, err := parseDLNASearch(criteria); err == nil {
			t.Errorf("accepted malformed/unsupported criteria %q", criteria)
		}
	}
}

func searchTestServer(t *testing.T) *mediaServer {
	t.Helper()
	bridge := testYouTubeBridge(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	bridge.ctx = ctx // Keep unit tests off speculative network lookups.
	catalog, err := newCatalog([]librarySource{bridge.source()})
	if err != nil {
		t.Fatal(err)
	}
	server := newMediaServer("Test", "test", "127.0.0.1:1339", net.IPv4(127, 0, 0, 1), catalog)
	server.youtube = bridge
	return server
}

func searchTestRequest(t *testing.T, server *mediaServer, rel, criteria, start, count, sort string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:Search xmlns:u="%s"><ContainerID>%s</ContainerID><SearchCriteria>%s</SearchCriteria><Filter>*</Filter><StartingIndex>%s</StartingIndex><RequestedCount>%s</RequestedCount><SortCriteria>%s</SortCriteria></u:Search></s:Body></s:Envelope>`, contentDirectoryURN, idFor(rel), escapeXML(criteria), start, count, sort)
	request := httptest.NewRequest(http.MethodPost, "/control/content", strings.NewReader(body))
	request.Header.Set("SOAPACTION", `"`+contentDirectoryURN+`#Search"`)
	request.RemoteAddr = "192.168.50.57:1234"
	response := httptest.NewRecorder()
	server.handleContentDirectory(response, request)
	return response
}

func TestDLNASearchResultsPagingAndPlayback(t *testing.T) {
	directory := mockYouTubeTools(t, map[string]string{"yt-dlp": `printf 'x\n' >> "$CALLS"
printf '%s' '{"entries":[{"id":"aaaaaaaaaaa","title":"Кот Альфа","duration":12},{"id":"bbbbbbbbbbb","title":"Кот Бета"},{"id":"ccccccccccc","title":"Dog"},{"id":"aaaaaaaaaaa","title":"Duplicate"},{"id":"../invalid","title":"Кот Invalid"}]}'
`})
	marker := filepath.Join(directory, "calls")
	t.Setenv("CALLS", marker)
	server := searchTestServer(t)
	criteria := `(upnp:class derivedfrom "object.item.videoItem") and dc:title contains "кот"`
	response := searchTestRequest(t, server, server.youtube.source().ID, criteria, "1", "1", "+dc:title")
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	values, err := decodeSOAPValues(response.Body, map[string]bool{"Result": true, "NumberReturned": true, "TotalMatches": true})
	if err != nil {
		t.Fatal(err)
	}
	if values["NumberReturned"] != "1" || values["TotalMatches"] != "2" || !strings.Contains(values["Result"], "Кот Бета") || !strings.Contains(values["Result"], "/youtube/bbbbbbbbbbb.ts") {
		t.Fatalf("wrong page: %v", values)
	}
	var didl struct {
		Items []struct {
			ID     string `xml:"id,attr"`
			Parent string `xml:"parentID,attr"`
		} `xml:"item"`
	}
	if err := xml.Unmarshal([]byte(values["Result"]), &didl); err != nil || len(didl.Items) != 1 {
		t.Fatalf("DIDL: %v", err)
	}
	for _, id := range []string{didl.Items[0].ID, didl.Items[0].Parent} {
		rel, err := relForID(id)
		if err != nil {
			t.Fatal(err)
		}
		entries, ok := server.youtube.entries(rel, true)
		if !ok || len(entries) != 1 {
			t.Fatal("search object is not browsable")
		}
	}
	head := httptest.NewRecorder()
	server.youtube.serve(head, httptest.NewRequest(http.MethodHead, "/youtube/bbbbbbbbbbb.ts", nil))
	if head.Code != 200 {
		t.Fatal("search video not playable")
	}
	response = searchTestRequest(t, server, ".", criteria, "0", "1", "")
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	calls, _ := os.ReadFile(marker)
	if string(calls) != "x\n" {
		t.Fatalf("pagination re-ran search: %q", calls)
	}
	if !strings.Contains(server.youtube.currentSearchStatus(), "192.168.50.57") {
		t.Fatal("missing TV request diagnostic")
	}
	files, err := os.ReadDir(server.youtube.root)
	if err != nil || len(files) != 0 {
		t.Fatal("search wrote disk cache")
	}
}

func TestDLNASearchRejectsUnsupportedRequests(t *testing.T) {
	server := searchTestServer(t)
	for _, check := range []struct {
		rel, criteria, start, sort string
		code                       string
	}{
		{".", `dc:creator contains "cat"`, "0", "", "708"},
		{".", `dc:title contains "cat"`, "-1", "", "402"},
		{".", `dc:title contains "cat"`, "0", "+unknown", "709"},
		{"missing", `*`, "0", "", "710"},
		{server.youtube.source().ID + "/recommended/jNQXAC9IVRw", `*`, "0", "", "710"},
	} {
		response := searchTestRequest(t, server, check.rel, check.criteria, check.start, "10", check.sort)
		if response.Code != 500 || !strings.Contains(response.Body.String(), "<errorCode>"+check.code+"</errorCode>") {
			t.Fatalf("incorrect SOAP error: %s", response.Body.String())
		}
	}
}

func TestDLNASearchAnnouncementAndClassOnlyRequests(t *testing.T) {
	server := searchTestServer(t)
	request := httptest.NewRequest(http.MethodPost, "/control/content", nil)
	request.Header.Set("SOAPACTION", contentDirectoryURN+"#GetSearchCapabilities")
	response := httptest.NewRecorder()
	server.handleContentDirectory(response, request)
	if !strings.Contains(response.Body.String(), "dc:title,upnp:class") {
		t.Fatal("missing search capabilities")
	}
	data, err := server.marshalDIDL([]mediaEntry{{Rel: ".", Name: "Root", Dir: true}, {Rel: server.youtube.source().ID, Name: "YouTube", Dir: true}})
	if err != nil || strings.Count(string(data), `searchable="1"`) != 2 || !strings.Contains(string(data), `<upnp:searchClass includeDerived="1">object.item.videoItem`) {
		t.Fatalf("search not announced: %s %v", data, err)
	}
	if !strings.Contains(contentDirectoryDescription, "<name>Search</name>") || !strings.Contains(contentDirectoryDescription, "<name>A_ARG_TYPE_SearchCriteria</name>") {
		t.Fatal("missing Search service declaration")
	}
	response = searchTestRequest(t, server, ".", `upnp:class derivedfrom "object.item.videoItem" and @refID exists false`, "0", "0", "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), "<NumberReturned>1</NumberReturned>") {
		t.Fatalf("class-only search failed: %s", response.Body.String())
	}
}

func TestYouTubeSearchBoundedCacheAndCancellation(t *testing.T) {
	mockYouTubeTools(t, map[string]string{"yt-dlp": `printf '%s' '{"entries":[{"id":"aaaaaaaaaaa","title":"Result"}]}'`})
	bridge := testYouTubeBridge(t)
	for i := 0; i < 10; i++ {
		if _, err := bridge.search(t.Context(), fmt.Sprintf("query %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if len(bridge.searches) != 8 {
		t.Fatal("unbounded query cache")
	}
	if _, ok := bridge.searches[youtubeSearchKey("query 0")]; ok {
		t.Fatal("oldest query not evicted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := bridge.search(ctx, "canceled"); err == nil {
		t.Fatal("canceled request succeeded")
	}
	for _, query := range []string{"", strings.Repeat("x", 201), "line\nbreak"} {
		if _, err := bridge.search(t.Context(), query); err == nil {
			t.Fatal("invalid query accepted")
		}
	}
}

func TestYouTubeSearchIntegration(t *testing.T) {
	if os.Getenv("LINN_YOUTUBE_SEARCH_TEST") != "1" {
		t.Skip("set LINN_YOUTUBE_SEARCH_TEST=1")
	}
	server := searchTestServer(t)
	started := time.Now()
	if os.Getenv("LINN_YOUTUBE_ACCOUNT_TEST") == "1" {
		cache, err := os.UserCacheDir()
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(cache, "FolderDLNA", "YouTube", youtubeAccountFile))
		if err != nil {
			t.Fatal(err)
		}
		server.youtube.session, err = youtubeBrowserSession(data)
		if err != nil {
			t.Fatal(err)
		}
	}
	response := searchTestRequest(t, server, server.youtube.source().ID, `dc:title contains "Big Buck Bunny" and upnp:class derivedfrom "object.item.videoItem"`, "0", "10", "")
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	values, err := decodeSOAPValues(response.Body, map[string]bool{"Result": true, "NumberReturned": true, "TotalMatches": true})
	if err != nil {
		t.Fatal(err)
	}
	if values["NumberReturned"] == "0" {
		t.Fatal("live YouTube search returned no playable results")
	}
	t.Logf("Live DLNA search: %s results, %s returned in %s", values["TotalMatches"], values["NumberReturned"], time.Since(started))
	var didl struct {
		Items []struct {
			ID       string `xml:"id,attr"`
			Resource string `xml:"res"`
		} `xml:"item"`
	}
	if err := xml.Unmarshal([]byte(values["Result"]), &didl); err != nil {
		t.Fatal(err)
	}
	rel, err := relForID(didl.Items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	id := rel[strings.LastIndex(rel, "/")+1:]
	httpServer := httptest.NewServer(http.HandlerFunc(server.youtube.serve))
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 70*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/youtube/"+id+".ts", nil)
	stream, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	packet := make([]byte, 188)
	if stream.StatusCode != 200 {
		t.Fatalf("search result playback HTTP %d", stream.StatusCode)
	}
	if _, err := io.ReadFull(stream.Body, packet); err != nil || packet[0] != 0x47 {
		t.Fatalf("invalid stream: %v", err)
	}
	t.Log("Search result playback returned MPEG-TS")
}
