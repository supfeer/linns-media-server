# Linn's media server

A minimal DLNA media server with a lightweight desktop interface for macOS, Windows, and Linux.

## Desktop application

```sh
go run .
```

Libraries added through the application window are saved in the user configuration directory. Closing the window hides the application in the system tray while the server continues running.

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

GitHub Actions runs the test suite and creates native packages for macOS, Windows, and Linux. Downloadable files are available in the artifacts of each `Build` workflow run.

File changes are applied after two identical background scans. The server then increments `SystemUpdateID`, sends `ContentDirectory` events, and performs an SSDP `byebye/alive` cycle while preserving the same UUID.
