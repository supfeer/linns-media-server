package main

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha1"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/ipv4"
)

const (
	mediaServerURN       = "urn:schemas-upnp-org:device:MediaServer:1"
	contentDirectoryURN  = "urn:schemas-upnp-org:service:ContentDirectory:1"
	connectionManagerURN = "urn:schemas-upnp-org:service:ConnectionManager:1"
	ssdpAddress          = "239.255.255.250:1900"
)

type mediaServer struct {
	name       string
	udn        string
	baseURL    string
	localIP    net.IP
	library    *library
	http       *http.Server
	subsMu     sync.Mutex
	subs       map[string]*subscription
	httpClient *http.Client
}

type subscription struct {
	sid      string
	callback *url.URL
	expires  time.Time
	sequence uint32
}

func newMediaServer(name, udn, address string, localIP net.IP, library *library) *mediaServer {
	server := &mediaServer{
		name:    name,
		udn:     "uuid:" + udn,
		baseURL: "http://" + address,
		localIP: localIP,
		library: library,
		subs:    make(map[string]*subscription),
		httpClient: &http.Client{
			Timeout: 3 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/device.xml", server.serveDeviceDescription)
	mux.HandleFunc("/cds.xml", func(writer http.ResponseWriter, request *http.Request) {
		serveXML(writer, request, contentDirectoryDescription)
	})
	mux.HandleFunc("/cms.xml", func(writer http.ResponseWriter, request *http.Request) {
		serveXML(writer, request, connectionManagerDescription)
	})
	mux.HandleFunc("/control/content", server.handleContentDirectory)
	mux.HandleFunc("/control/connection", server.handleConnectionManager)
	mux.HandleFunc("/event/content", server.handleEvent)
	mux.HandleFunc("/media/", server.serveMedia)
	server.http = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	return server
}

func (server *mediaServer) serve(listener net.Listener) error {
	return server.http.Serve(listener)
}

func (server *mediaServer) shutdown(ctx context.Context) error {
	return server.http.Shutdown(ctx)
}

func (server *mediaServer) deviceURL() string {
	return server.baseURL + "/device.xml"
}

func (server *mediaServer) serveDeviceDescription(writer http.ResponseWriter, request *http.Request) {
	body := fmt.Sprintf(deviceDescription, escapeXML(server.name), escapeXML(server.udn))
	serveXML(writer, request, body)
}

func serveXML(writer http.ResponseWriter, request *http.Request, body string) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writer.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
	writer.Header().Set("Server", serverHeader())
	if request.Method == http.MethodGet {
		_, _ = io.WriteString(writer, body)
	}
}

func (server *mediaServer) serveMedia(writer http.ResponseWriter, request *http.Request) {
	id, err := url.PathUnescape(strings.TrimPrefix(request.URL.Path, "/media/"))
	if err != nil || id == "" {
		http.NotFound(writer, request)
		return
	}
	file, entry, err := server.library.openMedia(id)
	if err != nil {
		http.NotFound(writer, request)
		return
	}
	defer file.Close()

	writer.Header().Set("Content-Type", mimeType(entry.Name))
	writer.Header().Set("transferMode.dlna.org", "Streaming")
	writer.Header().Set("contentFeatures.dlna.org", dlnaFeatures())
	http.ServeContent(writer, request, entry.Name, time.Unix(0, entry.ModUnix), file)
}

func (server *mediaServer) handleContentDirectory(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	action, err := requestAction(request)
	if err != nil {
		writeUPnPError(writer, 401, "Invalid Action")
		return
	}

	switch action {
	case "GetSearchCapabilities":
		writeSOAPResponse(writer, contentDirectoryURN, action, [][2]string{{"SearchCaps", ""}})
	case "GetSortCapabilities":
		writeSOAPResponse(writer, contentDirectoryURN, action, [][2]string{{"SortCaps", ""}})
	case "GetSystemUpdateID":
		writeSOAPResponse(writer, contentDirectoryURN, action, [][2]string{{"Id", strconv.FormatUint(uint64(server.library.systemUpdateID()), 10)}})
	case "Browse":
		server.handleBrowse(writer, request, action)
	default:
		writeUPnPError(writer, 401, "Invalid Action")
	}
}

func (server *mediaServer) handleBrowse(writer http.ResponseWriter, request *http.Request, action string) {
	values, err := decodeSOAPValues(request.Body, map[string]bool{
		"ObjectID": true, "BrowseFlag": true, "StartingIndex": true, "RequestedCount": true,
	})
	if err != nil {
		writeUPnPError(writer, 402, "Invalid Args")
		return
	}
	rel, err := relForID(values["ObjectID"])
	if err != nil {
		writeUPnPError(writer, 701, "No Such Object")
		return
	}
	entry, ok := server.library.entry(rel)
	if !ok {
		writeUPnPError(writer, 701, "No Such Object")
		return
	}

	var entries []mediaEntry
	switch values["BrowseFlag"] {
	case "BrowseMetadata":
		entries = []mediaEntry{entry}
	case "BrowseDirectChildren":
		if !entry.Dir {
			writeUPnPError(writer, 710, "No Such Container")
			return
		}
		entries, _ = server.library.children(rel)
	default:
		writeUPnPError(writer, 402, "Invalid Args")
		return
	}

	total := len(entries)
	start, err := nonnegativeInt(values["StartingIndex"])
	if err != nil {
		writeUPnPError(writer, 402, "Invalid Args")
		return
	}
	count, err := nonnegativeInt(values["RequestedCount"])
	if err != nil {
		writeUPnPError(writer, 402, "Invalid Args")
		return
	}
	if start > len(entries) {
		start = len(entries)
	}
	entries = entries[start:]
	if count != 0 && count < len(entries) {
		entries = entries[:count]
	}

	didl, err := server.marshalDIDL(entries)
	if err != nil {
		writeUPnPError(writer, 501, "Action Failed")
		return
	}
	writeSOAPResponse(writer, contentDirectoryURN, action, [][2]string{
		{"Result", string(didl)},
		{"NumberReturned", strconv.Itoa(len(entries))},
		{"TotalMatches", strconv.Itoa(total)},
		{"UpdateID", strconv.FormatUint(uint64(server.library.systemUpdateID()), 10)},
	})
}

func (server *mediaServer) handleConnectionManager(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	action, err := requestAction(request)
	if err != nil {
		writeUPnPError(writer, 401, "Invalid Action")
		return
	}
	switch action {
	case "GetProtocolInfo":
		protocols := make([]string, 0, len(videoMIMETypes))
		seen := make(map[string]bool)
		for _, mediaType := range videoMIMETypes {
			if !seen[mediaType] {
				protocols = append(protocols, "http-get:*:"+mediaType+":"+dlnaFeatures())
				seen[mediaType] = true
			}
		}
		sort.Strings(protocols)
		writeSOAPResponse(writer, connectionManagerURN, action, [][2]string{{"Source", strings.Join(protocols, ",")}, {"Sink", ""}})
	case "GetCurrentConnectionIDs":
		writeSOAPResponse(writer, connectionManagerURN, action, [][2]string{{"ConnectionIDs", ""}})
	case "GetCurrentConnectionInfo":
		writeSOAPResponse(writer, connectionManagerURN, action, [][2]string{
			{"RcsID", "-1"}, {"AVTransportID", "-1"}, {"ProtocolInfo", ""},
			{"PeerConnectionManager", ""}, {"PeerConnectionID", "-1"},
			{"Direction", "Output"}, {"Status", "OK"},
		})
	default:
		writeUPnPError(writer, 401, "Invalid Action")
	}
}

type didlDocument struct {
	XMLName    xml.Name        `xml:"DIDL-Lite"`
	XMLNS      string          `xml:"xmlns,attr"`
	XMLNSDC    string          `xml:"xmlns:dc,attr"`
	XMLNSUPnP  string          `xml:"xmlns:upnp,attr"`
	XMLNSDLNA  string          `xml:"xmlns:dlna,attr"`
	Containers []didlContainer `xml:"container"`
	Items      []didlItem      `xml:"item"`
}

type didlContainer struct {
	ID         string `xml:"id,attr"`
	ParentID   string `xml:"parentID,attr"`
	Restricted string `xml:"restricted,attr"`
	ChildCount int    `xml:"childCount,attr"`
	Title      string `xml:"dc:title"`
	Class      string `xml:"upnp:class"`
}

type didlItem struct {
	ID         string       `xml:"id,attr"`
	ParentID   string       `xml:"parentID,attr"`
	Restricted string       `xml:"restricted,attr"`
	Title      string       `xml:"dc:title"`
	Class      string       `xml:"upnp:class"`
	Date       string       `xml:"dc:date,omitempty"`
	Resource   didlResource `xml:"res"`
}

type didlResource struct {
	ProtocolInfo string `xml:"protocolInfo,attr"`
	Size         int64  `xml:"size,attr"`
	URL          string `xml:",chardata"`
}

func (server *mediaServer) marshalDIDL(entries []mediaEntry) ([]byte, error) {
	document := didlDocument{
		XMLNS:     "urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/",
		XMLNSDC:   "http://purl.org/dc/elements/1.1/",
		XMLNSUPnP: "urn:schemas-upnp-org:metadata-1-0/upnp/",
		XMLNSDLNA: "urn:schemas-dlna-org:metadata-1-0/",
	}
	for _, entry := range entries {
		parentID := idFor(parentRel(entry.Rel))
		if entry.Rel == "." {
			parentID = "-1"
		}
		if entry.Dir {
			document.Containers = append(document.Containers, didlContainer{
				ID: idFor(entry.Rel), ParentID: parentID, Restricted: "1",
				ChildCount: server.library.childCount(entry.Rel),
				Title:      entry.Name, Class: "object.container.storageFolder",
			})
			continue
		}
		id := idFor(entry.Rel)
		document.Items = append(document.Items, didlItem{
			ID: id, ParentID: parentID, Restricted: "1", Title: entry.Name,
			Class: "object.item.videoItem", Date: time.Unix(0, entry.ModUnix).Format("2006-01-02"),
			Resource: didlResource{
				ProtocolInfo: "http-get:*:" + mimeType(entry.Name) + ":" + dlnaFeatures(),
				Size:         entry.Size,
				URL:          server.baseURL + "/media/" + url.PathEscape(id),
			},
		})
	}
	return xml.Marshal(document)
}

func requestAction(request *http.Request) (string, error) {
	header := strings.Trim(request.Header.Get("SOAPACTION"), " \t\"")
	_, action, ok := strings.Cut(header, "#")
	if !ok || action == "" {
		return "", errors.New("invalid SOAPACTION")
	}
	return action, nil
}

func decodeSOAPValues(reader io.Reader, wanted map[string]bool) (map[string]string, error) {
	decoder := xml.NewDecoder(io.LimitReader(reader, 1<<20))
	values := make(map[string]string)
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return values, nil
		}
		if err != nil {
			return nil, err
		}
		start, ok := token.(xml.StartElement)
		if !ok || !wanted[start.Name.Local] {
			continue
		}
		var value string
		if err := decoder.DecodeElement(&value, &start); err != nil {
			return nil, err
		}
		values[start.Name.Local] = value
	}
}

func nonnegativeInt(value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return 0, errors.New("invalid nonnegative integer")
	}
	return parsed, nil
}

func writeSOAPResponse(writer http.ResponseWriter, service, action string, arguments [][2]string) {
	var body strings.Builder
	body.WriteString(`<?xml version="1.0" encoding="utf-8"?>`)
	body.WriteString(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	body.WriteString(`<u:` + action + `Response xmlns:u="` + service + `">`)
	for _, argument := range arguments {
		body.WriteString("<" + argument[0] + ">" + escapeXML(argument[1]) + "</" + argument[0] + ">")
	}
	body.WriteString(`</u:` + action + `Response></s:Body></s:Envelope>`)
	writeSOAP(writer, http.StatusOK, body.String())
}

func writeUPnPError(writer http.ResponseWriter, code int, description string) {
	body := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><s:Fault><faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0"><errorCode>%d</errorCode><errorDescription>%s</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`, code, escapeXML(description))
	writeSOAP(writer, http.StatusInternalServerError, body)
}

func writeSOAP(writer http.ResponseWriter, status int, body string) {
	writer.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
	writer.Header().Set("EXT", "")
	writer.Header().Set("Server", serverHeader())
	writer.WriteHeader(status)
	_, _ = io.WriteString(writer, body)
}

func escapeXML(value string) string {
	var escaped bytes.Buffer
	_ = xml.EscapeText(&escaped, []byte(value))
	return escaped.String()
}

func dlnaFeatures() string {
	return "DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000"
}

func serverHeader() string {
	return runtime.GOOS + "/" + runtime.Version() + " UPnP/1.0 LinnsMediaServer/0.1 DLNADOC/1.50"
}

func stableUUID(seed string) string {
	sum := sha1.Sum([]byte(seed))
	bytes := sum[:16]
	bytes[6] = (bytes[6] & 0x0f) | 0x50
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return formatUUID(bytes)
}

func formatUUID(value []byte) string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
}

func chooseInterface(name string) (*net.Interface, net.IP, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, nil, fmt.Errorf("list network interfaces: %w", err)
	}
	for index := range interfaces {
		networkInterface := &interfaces[index]
		if name != "" && networkInterface.Name != name {
			continue
		}
		if networkInterface.Flags&net.FlagUp == 0 || networkInterface.Flags&net.FlagMulticast == 0 || networkInterface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, err := networkInterface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err == nil && ip.To4() != nil && ip.IsPrivate() {
				return networkInterface, ip.To4(), nil
			}
		}
	}
	if name != "" {
		return nil, nil, fmt.Errorf("interface %q has no active private IPv4 multicast address", name)
	}
	return nil, nil, errors.New("no active private IPv4 multicast interface found")
}

func (server *mediaServer) handleEvent(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case "SUBSCRIBE":
		server.subscribe(writer, request)
	case "UNSUBSCRIBE":
		server.unsubscribe(writer, request)
	default:
		writer.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (server *mediaServer) subscribe(writer http.ResponseWriter, request *http.Request) {
	timeout := subscriptionTimeout(request.Header.Get("TIMEOUT"))
	sid := request.Header.Get("SID")
	if sid != "" {
		server.subsMu.Lock()
		subscription, ok := server.subs[sid]
		if ok {
			subscription.expires = time.Now().Add(timeout)
		}
		server.subsMu.Unlock()
		if !ok {
			writer.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		writer.Header().Set("SID", sid)
		writer.Header().Set("TIMEOUT", "Second-"+strconv.Itoa(int(timeout.Seconds())))
		writer.WriteHeader(http.StatusOK)
		return
	}

	callback, err := validatedCallback(request.Header.Get("CALLBACK"))
	if err != nil || !strings.EqualFold(request.Header.Get("NT"), "upnp:event") {
		writer.WriteHeader(http.StatusPreconditionFailed)
		return
	}
	random := make([]byte, 16)
	if _, err := cryptorand.Read(random); err != nil {
		writer.WriteHeader(http.StatusInternalServerError)
		return
	}
	random[6] = (random[6] & 0x0f) | 0x40
	random[8] = (random[8] & 0x3f) | 0x80
	sid = "uuid:" + formatUUID(random)
	subscription := &subscription{sid: sid, callback: callback, expires: time.Now().Add(timeout)}
	server.subsMu.Lock()
	server.subs[sid] = subscription
	server.subsMu.Unlock()

	writer.Header().Set("SID", sid)
	writer.Header().Set("TIMEOUT", "Second-"+strconv.Itoa(int(timeout.Seconds())))
	writer.WriteHeader(http.StatusOK)
	go server.sendInitialNotification(sid)
}

func (server *mediaServer) unsubscribe(writer http.ResponseWriter, request *http.Request) {
	sid := request.Header.Get("SID")
	server.subsMu.Lock()
	_, ok := server.subs[sid]
	delete(server.subs, sid)
	server.subsMu.Unlock()
	if !ok {
		writer.WriteHeader(http.StatusPreconditionFailed)
		return
	}
	writer.WriteHeader(http.StatusOK)
}

func subscriptionTimeout(header string) time.Duration {
	const defaultTimeout = 30 * time.Minute
	if strings.EqualFold(header, "Second-infinite") {
		return defaultTimeout
	}
	seconds, err := strconv.Atoi(strings.TrimPrefix(header, "Second-"))
	if err != nil || seconds < 60 {
		return defaultTimeout
	}
	if seconds > int(defaultTimeout.Seconds()) {
		seconds = int(defaultTimeout.Seconds())
	}
	return time.Duration(seconds) * time.Second
}

func validatedCallback(header string) (*url.URL, error) {
	start := strings.IndexByte(header, '<')
	end := strings.IndexByte(header, '>')
	if start == -1 || end <= start+1 {
		return nil, errors.New("invalid callback")
	}
	callback, err := url.Parse(header[start+1 : end])
	if err != nil || callback.Scheme != "http" || callback.Hostname() == "" {
		return nil, errors.New("invalid callback")
	}
	ip := net.ParseIP(callback.Hostname())
	if ip == nil || (!ip.IsPrivate() && !ip.IsLoopback()) {
		return nil, errors.New("callback must use a private IP address")
	}
	return callback, nil
}

func (server *mediaServer) sendInitialNotification(sid string) {
	time.Sleep(50 * time.Millisecond)
	server.subsMu.Lock()
	subscription, ok := server.subs[sid]
	if !ok {
		server.subsMu.Unlock()
		return
	}
	notification := notificationFor(subscription)
	server.subsMu.Unlock()
	server.sendNotification(notification, server.library.currentChange())
}

func (server *mediaServer) notify(change libraryChange) {
	server.subsMu.Lock()
	now := time.Now()
	notifications := make([]eventNotification, 0, len(server.subs))
	for sid, subscription := range server.subs {
		if now.After(subscription.expires) {
			delete(server.subs, sid)
			continue
		}
		notifications = append(notifications, notificationFor(subscription))
	}
	server.subsMu.Unlock()
	for _, notification := range notifications {
		notification := notification
		go server.sendNotification(notification, change)
	}
}

type eventNotification struct {
	sid      string
	callback string
	sequence uint32
}

func notificationFor(subscription *subscription) eventNotification {
	notification := eventNotification{
		sid: subscription.sid, callback: subscription.callback.String(), sequence: subscription.sequence,
	}
	subscription.sequence++
	return notification
}

func (server *mediaServer) sendNotification(notification eventNotification, change libraryChange) {
	containerUpdates := make([]string, 0, len(change.Containers)*2)
	containerIDs := make([]string, 0, len(change.Containers))
	for rel := range change.Containers {
		containerIDs = append(containerIDs, rel)
	}
	sort.Strings(containerIDs)
	for _, rel := range containerIDs {
		containerUpdates = append(containerUpdates, idFor(rel), strconv.FormatUint(uint64(change.Containers[rel]), 10))
	}
	body := `<?xml version="1.0"?><e:propertyset xmlns:e="urn:schemas-upnp-org:event-1-0">` +
		`<e:property><SystemUpdateID>` + strconv.FormatUint(uint64(change.SystemID), 10) + `</SystemUpdateID></e:property>` +
		`<e:property><ContainerUpdateIDs>` + strings.Join(containerUpdates, ",") + `</ContainerUpdateIDs></e:property>` +
		`</e:propertyset>`
	request, err := http.NewRequest("NOTIFY", notification.callback, strings.NewReader(body))
	if err != nil {
		return
	}
	request.Header.Set("CONTENT-TYPE", `text/xml; charset="utf-8"`)
	request.Header.Set("NT", "upnp:event")
	request.Header.Set("NTS", "upnp:propchange")
	request.Header.Set("SID", notification.sid)
	request.Header.Set("SEQ", strconv.FormatUint(uint64(notification.sequence), 10))
	response, err := server.httpClient.Do(request)
	if err != nil {
		slog.Debug("event notification failed", "callback", notification.callback, "error", err)
		return
	}
	response.Body.Close()
}

type ssdpTarget struct {
	typeName string
	usn      string
}

type ssdpServer struct {
	group      *net.UDPAddr
	receiver   *net.UDPConn
	sender     *net.UDPConn
	location   string
	udn        string
	targets    []ssdpTarget
	announceMu sync.Mutex
	sendMu     sync.Mutex
}

func newSSDP(networkInterface *net.Interface, location, udn string) (*ssdpServer, error) {
	group, err := net.ResolveUDPAddr("udp4", ssdpAddress)
	if err != nil {
		return nil, err
	}
	receiver, err := net.ListenMulticastUDP("udp4", networkInterface, group)
	if err != nil {
		return nil, fmt.Errorf("listen for SSDP on %s: %w", networkInterface.Name, err)
	}
	if err := configureSSDPSocket(receiver, networkInterface); err != nil {
		receiver.Close()
		return nil, err
	}
	qualifiedUDN := "uuid:" + udn
	return &ssdpServer{
		group: group, receiver: receiver, sender: receiver,
		location: location, udn: qualifiedUDN,
		targets: []ssdpTarget{
			{typeName: "upnp:rootdevice", usn: qualifiedUDN + "::upnp:rootdevice"},
			{typeName: qualifiedUDN, usn: qualifiedUDN},
			{typeName: mediaServerURN, usn: qualifiedUDN + "::" + mediaServerURN},
			{typeName: contentDirectoryURN, usn: qualifiedUDN + "::" + contentDirectoryURN},
			{typeName: connectionManagerURN, usn: qualifiedUDN + "::" + connectionManagerURN},
		},
	}, nil
}

func configureSSDPSocket(socket *net.UDPConn, networkInterface *net.Interface) error {
	if err := ipv4.NewPacketConn(socket).SetMulticastInterface(networkInterface); err != nil {
		return fmt.Errorf("set SSDP multicast interface %s: %w", networkInterface.Name, err)
	}
	return nil
}

func (server *ssdpServer) run(ctx context.Context) {
	readDone := make(chan struct{})
	go func() {
		server.readSearches(ctx)
		close(readDone)
	}()
	server.advertise("ssdp:alive")
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			server.advertise("ssdp:byebye")
			server.receiver.Close()
			<-readDone
			if server.sender != server.receiver {
				server.sender.Close()
			}
			return
		case <-ticker.C:
			server.advertise("ssdp:alive")
		}
	}
}

func (server *ssdpServer) republish(ctx context.Context) {
	server.announceMu.Lock()
	defer server.announceMu.Unlock()
	server.advertiseLocked("ssdp:byebye")
	select {
	case <-ctx.Done():
		return
	case <-time.After(500 * time.Millisecond):
	}
	server.advertiseLocked("ssdp:alive")
}

func (server *ssdpServer) advertise(kind string) {
	server.announceMu.Lock()
	defer server.announceMu.Unlock()
	server.advertiseLocked(kind)
}

func (server *ssdpServer) advertiseLocked(kind string) {
	for repeat := 0; repeat < 3; repeat++ {
		for _, target := range server.targets {
			var message strings.Builder
			message.WriteString("NOTIFY * HTTP/1.1\r\n")
			message.WriteString("HOST: " + ssdpAddress + "\r\n")
			if kind == "ssdp:alive" {
				message.WriteString("CACHE-CONTROL: max-age=1800\r\n")
				message.WriteString("LOCATION: " + server.location + "\r\n")
				message.WriteString("SERVER: " + serverHeader() + "\r\n")
			}
			message.WriteString("NT: " + target.typeName + "\r\n")
			message.WriteString("NTS: " + kind + "\r\n")
			message.WriteString("USN: " + target.usn + "\r\n\r\n")
			server.writePacket([]byte(message.String()), server.group)
		}
		if repeat < 2 {
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func (server *ssdpServer) readSearches(ctx context.Context) {
	buffer := make([]byte, 64<<10)
	for {
		_ = server.receiver.SetReadDeadline(time.Now().Add(time.Second))
		count, address, err := server.receiver.ReadFromUDP(buffer)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if networkError, ok := err.(net.Error); ok && networkError.Timeout() {
				continue
			}
			return
		}
		headers, ok := parseSearch(buffer[:count])
		if !ok {
			continue
		}
		go server.respondToSearch(ctx, address, headers)
	}
}

func parseSearch(packet []byte) (map[string]string, bool) {
	lines := strings.Split(strings.ReplaceAll(string(packet), "\r\n", "\n"), "\n")
	if len(lines) == 0 || !strings.EqualFold(strings.TrimSpace(lines[0]), "M-SEARCH * HTTP/1.1") {
		return nil, false
	}
	headers := make(map[string]string)
	for _, line := range lines[1:] {
		name, value, ok := strings.Cut(line, ":")
		if ok {
			headers[strings.ToUpper(strings.TrimSpace(name))] = strings.TrimSpace(value)
		}
	}
	if !strings.Contains(strings.ToLower(headers["MAN"]), "ssdp:discover") || headers["ST"] == "" {
		return nil, false
	}
	return headers, true
}

func (server *ssdpServer) respondToSearch(ctx context.Context, address *net.UDPAddr, headers map[string]string) {
	mx, _ := strconv.Atoi(headers["MX"])
	if mx < 1 {
		mx = 1
	}
	if mx > 2 {
		mx = 2
	}
	delay := time.Duration(rand.IntN(mx*1000)) * time.Millisecond
	select {
	case <-ctx.Done():
		return
	case <-time.After(delay):
	}
	for _, target := range server.targets {
		if headers["ST"] != "ssdp:all" && !strings.EqualFold(headers["ST"], target.typeName) {
			continue
		}
		message := "HTTP/1.1 200 OK\r\n" +
			"CACHE-CONTROL: max-age=1800\r\n" +
			"DATE: " + time.Now().UTC().Format(http.TimeFormat) + "\r\n" +
			"EXT:\r\n" +
			"LOCATION: " + server.location + "\r\n" +
			"SERVER: " + serverHeader() + "\r\n" +
			"ST: " + target.typeName + "\r\n" +
			"USN: " + target.usn + "\r\n\r\n"
		server.writePacket([]byte(message), address)
	}
}

func (server *ssdpServer) writePacket(packet []byte, address *net.UDPAddr) {
	server.sendMu.Lock()
	defer server.sendMu.Unlock()
	_, _ = server.sender.WriteToUDP(packet, address)
}

const deviceDescription = `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0" xmlns:dlna="urn:schemas-dlna-org:device-1-0">
  <specVersion><major>1</major><minor>0</minor></specVersion>
  <device>
    <deviceType>urn:schemas-upnp-org:device:MediaServer:1</deviceType>
    <friendlyName>%s</friendlyName>
    <manufacturer>Linn's media server</manufacturer>
    <modelDescription>Minimal folder DLNA server</modelDescription>
    <modelName>Linn's media server</modelName>
    <modelNumber>0.1</modelNumber>
    <UDN>%s</UDN>
    <dlna:X_DLNADOC>DMS-1.50</dlna:X_DLNADOC>
    <serviceList>
      <service>
        <serviceType>urn:schemas-upnp-org:service:ContentDirectory:1</serviceType>
        <serviceId>urn:upnp-org:serviceId:ContentDirectory</serviceId>
        <SCPDURL>/cds.xml</SCPDURL>
        <controlURL>/control/content</controlURL>
        <eventSubURL>/event/content</eventSubURL>
      </service>
      <service>
        <serviceType>urn:schemas-upnp-org:service:ConnectionManager:1</serviceType>
        <serviceId>urn:upnp-org:serviceId:ConnectionManager</serviceId>
        <SCPDURL>/cms.xml</SCPDURL>
        <controlURL>/control/connection</controlURL>
        <eventSubURL></eventSubURL>
      </service>
    </serviceList>
  </device>
</root>`

const contentDirectoryDescription = `<?xml version="1.0"?>
<scpd xmlns="urn:schemas-upnp-org:service-1-0">
  <specVersion><major>1</major><minor>0</minor></specVersion>
  <actionList>
    <action><name>GetSearchCapabilities</name><argumentList><argument><name>SearchCaps</name><direction>out</direction><relatedStateVariable>SearchCapabilities</relatedStateVariable></argument></argumentList></action>
    <action><name>GetSortCapabilities</name><argumentList><argument><name>SortCaps</name><direction>out</direction><relatedStateVariable>SortCapabilities</relatedStateVariable></argument></argumentList></action>
    <action><name>GetSystemUpdateID</name><argumentList><argument><name>Id</name><direction>out</direction><relatedStateVariable>SystemUpdateID</relatedStateVariable></argument></argumentList></action>
    <action><name>Browse</name><argumentList>
      <argument><name>ObjectID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_ObjectID</relatedStateVariable></argument>
      <argument><name>BrowseFlag</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_BrowseFlag</relatedStateVariable></argument>
      <argument><name>Filter</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_Filter</relatedStateVariable></argument>
      <argument><name>StartingIndex</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_Index</relatedStateVariable></argument>
      <argument><name>RequestedCount</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_Count</relatedStateVariable></argument>
      <argument><name>SortCriteria</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_SortCriteria</relatedStateVariable></argument>
      <argument><name>Result</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_Result</relatedStateVariable></argument>
      <argument><name>NumberReturned</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_Count</relatedStateVariable></argument>
      <argument><name>TotalMatches</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_Count</relatedStateVariable></argument>
      <argument><name>UpdateID</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_UpdateID</relatedStateVariable></argument>
    </argumentList></action>
  </actionList>
  <serviceStateTable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_ObjectID</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_BrowseFlag</name><dataType>string</dataType><allowedValueList><allowedValue>BrowseMetadata</allowedValue><allowedValue>BrowseDirectChildren</allowedValue></allowedValueList></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_Filter</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_SortCriteria</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_Index</name><dataType>ui4</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_Count</name><dataType>ui4</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_UpdateID</name><dataType>ui4</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_Result</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>SearchCapabilities</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>SortCapabilities</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="yes"><name>SystemUpdateID</name><dataType>ui4</dataType></stateVariable>
    <stateVariable sendEvents="yes"><name>ContainerUpdateIDs</name><dataType>string</dataType></stateVariable>
  </serviceStateTable>
</scpd>`

const connectionManagerDescription = `<?xml version="1.0"?>
<scpd xmlns="urn:schemas-upnp-org:service-1-0">
  <specVersion><major>1</major><minor>0</minor></specVersion>
  <actionList>
    <action><name>GetProtocolInfo</name><argumentList><argument><name>Source</name><direction>out</direction><relatedStateVariable>SourceProtocolInfo</relatedStateVariable></argument><argument><name>Sink</name><direction>out</direction><relatedStateVariable>SinkProtocolInfo</relatedStateVariable></argument></argumentList></action>
    <action><name>GetCurrentConnectionIDs</name><argumentList><argument><name>ConnectionIDs</name><direction>out</direction><relatedStateVariable>CurrentConnectionIDs</relatedStateVariable></argument></argumentList></action>
    <action><name>GetCurrentConnectionInfo</name><argumentList>
      <argument><name>ConnectionID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_ConnectionID</relatedStateVariable></argument>
      <argument><name>RcsID</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_RcsID</relatedStateVariable></argument>
      <argument><name>AVTransportID</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_AVTransportID</relatedStateVariable></argument>
      <argument><name>ProtocolInfo</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_ProtocolInfo</relatedStateVariable></argument>
      <argument><name>PeerConnectionManager</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_ConnectionManager</relatedStateVariable></argument>
      <argument><name>PeerConnectionID</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_ConnectionID</relatedStateVariable></argument>
      <argument><name>Direction</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_Direction</relatedStateVariable></argument>
      <argument><name>Status</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_ConnectionStatus</relatedStateVariable></argument>
    </argumentList></action>
  </actionList>
  <serviceStateTable>
    <stateVariable sendEvents="no"><name>SourceProtocolInfo</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>SinkProtocolInfo</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>CurrentConnectionIDs</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_ConnectionStatus</name><dataType>string</dataType><allowedValueList><allowedValue>OK</allowedValue><allowedValue>ContentFormatMismatch</allowedValue><allowedValue>InsufficientBandwidth</allowedValue><allowedValue>UnreliableChannel</allowedValue><allowedValue>Unknown</allowedValue></allowedValueList></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_ConnectionManager</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_Direction</name><dataType>string</dataType><allowedValueList><allowedValue>Input</allowedValue><allowedValue>Output</allowedValue></allowedValueList></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_ProtocolInfo</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_ConnectionID</name><dataType>i4</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_AVTransportID</name><dataType>i4</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_RcsID</name><dataType>i4</dataType></stateVariable>
  </serviceStateTable>
</scpd>`
