# Linn's media server

A minimal DLNA media server with a lightweight desktop interface for macOS, Windows, and Linux.

## Desktop application

```sh
go run .
```

Libraries added through the application window are saved in the user configuration directory. Closing the window hides the application in the system tray while the server continues running.

A normal launch opens the main window. Login autostart uses `--background` to stay in the system tray.

## YouTube to DLNA

On macOS, click **Sign in with browser** in the server window. It opens Yandex Browser with connection instructions. Once, load the included **Linn YouTube Connect** extension through `browser://extensions` → Developer mode → Load unpacked (the connection page shows the folder). Sign in to YouTube, return to the connection tab, and click the extension icon → **Connect YouTube**. The browser can then be closed. The TV's YouTube folder contains Recommendations, Subscriptions, Watch later and Liked videos. Lists refresh at startup and every 15 minutes; the button refreshes them immediately. Each list currently contains up to 60 videos.

Browsing loads titles and thumbnails and prepares media addresses for the first three videos on the current page. Video is fetched only when the TV requests playback and is streamed as HEVC/AAC or H.264/AAC MPEG-TS through FFmpeg. No video file is saved. Thumbnails also pass through the Mac, so the TV only needs LAN access. The Mac must stay awake and retain access to YouTube (including its VPN, when needed).

Unprepared media addresses can take around 15 seconds to resolve. They are kept in memory for five minutes; page changes cancel unrelated preparation, and playback takes priority. HEAD probes do not start video retrieval. At most two streams run at once; closing a connection or stopping the server cancels its streaming processes. Seeking is not supported. Age-restricted, live and members-only playback has not been verified.

The main window has ordered codec and resolution pickers. Selecting an occupied priority swaps the two positions; changes are saved and apply to the next video. Defaults are HEVC → H.264 and 2160p (4K) → 1440p → 1080p → 720p → 480p. The first available resolution tier wins, without upscaling. Matching video codecs are copied; other sources use VideoToolbox on macOS and software encoding elsewhere. SDR is preferred. Public video extraction runs without account cookies first to avoid account-specific 360p restrictions, with an authenticated retry when necessary. Personal lists still use the connected account.

The next codec is tried if the encoder fails before any media bytes are sent. Once streaming starts, changing codecs inside the response is unsafe. A TV reading bytes does not confirm successful decoding, so Linn does not record speculative device compatibility from HTTP traffic. If a TV rejects HEVC, move H.264 first. Playback must still be checked on each device; 4K encoding performance depends on the server hardware.

Experimental native DLNA search is implemented on the server; TV compatibility is not established. Linn announces `Search`, `dc:title`, `upnp:class`, and searchable containers; `and`/`or`, parentheses, class filters and pagination are supported. A title search from the server root or YouTube folder retrieves up to 30 public YouTube candidates per term (at most four terms), applies the DLNA filter, and returns playable video items. Root searches also include matching local media. Searches inside an existing feed only filter that feed. The first query may take 10–25 seconds; subsequent pages reuse a five-minute cache. At most eight query results remain in memory; video files are never written to disk. The bounded diagnostic log described below includes search parameters. Class-only searches enumerate the available catalogue without issuing an online text search.

Known limitation: search does not work on the tested LG webOS TV. During diagnosis it requested search capabilities and browsed the catalogue, but no `Search` command reached Linn. Server-side search and playback tests pass; this does not establish that the TV forwards text queries. This remains unresolved.

The main window shows the last DLNA search request's client IP, success count or error code. This confirms whether the TV actually sent a Search request. A television that only supports browsing cannot gain a search field through a server update; no folder keyboard or web search UI is added.

Install `yt-dlp`, `ffmpeg`, and Node.js 22 or newer. The app looks on `PATH`, in the `tools` directory beside its configuration file, and in `/opt/homebrew/bin` and `/usr/local/bin`. On macOS the app-specific directory is `~/Library/Application Support/FolderDLNA/tools`. Tools installed elsewhere can be linked into that directory. Update yt-dlp when YouTube changes its delivery format.

The app does not read the browser database or Keychain. The extension reads only YouTube cookies when you press its Connect button, then sends them to a temporary loopback listener with a random one-use connection token. Sessions are stored in the app's private cache directory (mode 0600), used through temporary files for yt-dlp, and never written to logs. To reconnect or change accounts, use **Sign in with browser** again. Delete `~/Library/Caches/FolderDLNA/YouTube/.youtube-account.json` while the app is stopped to forget the connection. Login waits at most 10 minutes; each list request waits at most 45 seconds, and the UI supports cancellation and shows the current list. Lists become available as they finish. No direct OAuth integration is used: the public YouTube API does not provide the personal Home recommendations feed.

Optional integration tests (the account test uses the session already connected through the extension):

```sh
LINN_YOUTUBE_STREAM_TEST=1 go test -run TestYouTubeStreamingIntegration -v
LINN_YOUTUBE_4K_TEST=1 go test -run TestYouTube4KStreamingIntegration -v
LINN_YOUTUBE_SEARCH_TEST=1 LINN_YOUTUBE_ACCOUNT_TEST=1 go test -run TestYouTubeSearchIntegration -v
LINN_YOUTUBE_ACCOUNT_TEST=1 go test -run TestYouTubeAccountIntegration -v
```

## Headless mode

```sh
go run . -media "/path/to/videos" -name "My Videos"
```

## Local installation

```sh
go install fyne.io/tools/cmd/fyne@latest
fyne install
```

## Builds

For TV search troubleshooting, `FolderDLNA/dlna-requests.json` in the OS user cache directory keeps the latest 64 ContentDirectory events from the current server run. It records client addresses, Browse/Search parameters, HTTP status and timings. On macOS this is `~/Library/Caches/FolderDLNA/dlna-requests.json`. The private file is overwritten as new requests arrive; account credentials, video URLs and response bodies are excluded.

GitHub Actions runs the test suite and creates native packages for macOS, Windows, and Linux. Downloadable files are available in the artifacts of each `Build` workflow run.

File changes are applied after two identical background scans. The server then increments `SystemUpdateID`, sends `ContentDirectory` events, and repeats its SSDP `alive` announcement. `byebye` is reserved for shutdown, so refreshing a list does not disconnect the TV.

YouTube lists are cached as small metadata files and restored at startup. An empty recommendation response or an expired session does not erase the last successful lists. YouTube may rotate the browser session; if Linn asks you to reconnect, use **Sign in with browser** again. Only list metadata is cached, never video files.
