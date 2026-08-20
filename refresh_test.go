package main

import (
	"context"
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

type receivedEvent struct {
	body     string
	sid      string
	sequence string
}

func TestFileChangeNotifiesAndReadvertises(t *testing.T) {
	root := t.TempDir()
	library, err := newLibrary(root)
	if err != nil {
		t.Fatal(err)
	}

	events := make(chan receivedEvent, 2)
	callback := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
			return
		}
		writer.WriteHeader(http.StatusOK)
		events <- receivedEvent{
			body: string(body), sid: request.Header.Get("SID"), sequence: request.Header.Get("SEQ"),
		}
	}))
	defer callback.Close()

	media := newMediaServer("Test", stableUUID(root), "127.0.0.1:1338", net.ParseIP("127.0.0.1"), library)
	subscribe := httptest.NewRequest("SUBSCRIBE", "/event/content", nil)
	subscribe.Header.Set("CALLBACK", "<"+callback.URL+">")
	subscribe.Header.Set("NT", "upnp:event")
	recorder := httptest.NewRecorder()
	media.handleEvent(recorder, subscribe)
	if recorder.Code != http.StatusOK {
		t.Fatalf("subscribe status: %d", recorder.Code)
	}
	initial := waitForEvent(t, events)
	if !strings.Contains(initial.body, "<SystemUpdateID>1</SystemUpdateID>") || initial.sequence != "0" {
		t.Fatalf("unexpected initial event: %#v", initial)
	}

	udpReceiver, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer udpReceiver.Close()
	udpSender, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer udpSender.Close()
	discovery := &ssdpServer{
		group: udpReceiver.LocalAddr().(*net.UDPAddr),
		sender: udpSender,
		location: "http://127.0.0.1:1338/device.xml",
		udn: "uuid:test",
		targets: []ssdpTarget{{typeName: "upnp:rootdevice", usn: "uuid:test::upnp:rootdevice"}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changes := make(chan libraryChange, 2)
	go library.watch(ctx, 10*time.Millisecond, func(change libraryChange) {
		media.notify(change)
		discovery.republish(ctx)
		changes <- change
	})
	if err := os.WriteFile(filepath.Join(root, "New Movie.mp4"), []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}

	updated := waitForEvent(t, events)
	if !strings.Contains(updated.body, "<SystemUpdateID>2</SystemUpdateID>") ||
		!strings.Contains(updated.body, "<ContainerUpdateIDs>0,2</ContainerUpdateIDs>") ||
		updated.sid != initial.sid || updated.sequence != "1" {
		t.Fatalf("unexpected update event: %#v", updated)
	}

	if err := udpReceiver.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	packets := make([]string, 0, 6)
	buffer := make([]byte, 4096)
	for len(packets) < 6 {
		count, _, err := udpReceiver.ReadFromUDP(buffer)
		if err != nil {
			t.Fatalf("read SSDP packet %d: %v", len(packets)+1, err)
		}
		packets = append(packets, string(buffer[:count]))
	}
	for index, packet := range packets {
		expectedKind := "ssdp:byebye"
		if index >= 3 {
			expectedKind = "ssdp:alive"
		}
		if !strings.Contains(packet, "NTS: "+expectedKind) ||
			!strings.Contains(packet, "USN: uuid:test::upnp:rootdevice") {
			t.Fatalf("unexpected SSDP packet %d: %q", index+1, packet)
		}
	}

	change := <-changes
	if change.SystemID != 2 || change.Containers["."] != 2 {
		t.Fatalf("unexpected library change: %#v", change)
	}
	children, ok := library.children(".")
	if !ok || len(children) != 1 || children[0].Name != "New Movie.mp4" {
		t.Fatalf("updated file is not published: %#v", children)
	}
	select {
	case duplicate := <-changes:
		t.Fatalf("unchanged library was published again: %#v", duplicate)
	case <-time.After(50 * time.Millisecond):
	}
}

func waitForEvent(t *testing.T, events <-chan receivedEvent) receivedEvent {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for GENA event")
		return receivedEvent{}
	}
}
