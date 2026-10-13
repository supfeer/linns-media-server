package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDLNARequestTrace(t *testing.T) {
	server := searchTestServer(t)
	server.trace = &dlnaTrace{file: filepath.Join(t.TempDir(), "requests.json")}
	request := httptest.NewRequest(http.MethodPost, "/control/content", strings.NewReader(`<Envelope><Body><Browse><ObjectID>0</ObjectID><BrowseFlag>BrowseDirectChildren</BrowseFlag><StartingIndex>0</StartingIndex><RequestedCount>10</RequestedCount></Browse></Body></Envelope>`))
	request.Header.Set("SOAPACTION", contentDirectoryURN+"#Browse")
	request.Header.Set("Cookie", "secret-cookie")
	request.Header.Set("Authorization", "secret-token")
	request.RemoteAddr = "192.168.50.57:1000"
	response := httptest.NewRecorder()
	server.handleContentDirectory(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "YouTube") {
		t.Fatalf("tracing changed Browse response: %s", response.Body.String())
	}
	searchTestRequest(t, server, ".", `dc:creator contains "cat"`, "0", "10", "")
	data, err := os.ReadFile(server.trace.file)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"action": "Browse"`, `"ObjectID": "0"`, `"action": "Search"`, `"SearchCriteria":`, `"upnp_error": "708"`, `"http_status": "500"`, `192.168.50.57`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("trace missing %s", want)
		}
	}
	if strings.Contains(string(data), "secret-") || strings.Contains(string(data), "DIDL-Lite") {
		t.Fatal("trace leaked headers or response body")
	}
	info, err := os.Stat(server.trace.file)
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
		t.Fatalf("diagnostic file permissions: %v, %v", info, err)
	}
	for i := 0; i < 70; i++ {
		server.trace.record(request, "Browse", map[string]string{"index": fmt.Sprint(i), "agent": strings.Repeat("x", 5000)})
	}
	data, err = os.ReadFile(server.trace.file)
	if err != nil {
		t.Fatal(err)
	}
	var events []map[string]string
	if err := json.Unmarshal(data, &events); err != nil {
		t.Fatal(err)
	}
	if len(events) != 64 || events[0]["index"] != "6" || events[63]["index"] != "69" || len(events[0]["agent"]) > 2051 {
		t.Fatal("diagnostics are not bounded")
	}
}
