package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

type desktopController struct {
	ctx           context.Context
	interfaceName string
	scanInterval  time.Duration
	running       *runningServer
	youtube       *youtubeBridge
}

func (controller *desktopController) restart(config appConfig) error {
	if controller.running != nil {
		controller.running.Stop()
		_ = controller.running.Wait()
		controller.running = nil
	}
	available := make([]librarySource, 0, len(config.Libraries))
	for _, library := range config.Libraries {
		if info, err := os.Stat(library.Path); err == nil && info.IsDir() {
			available = append(available, library)
		}
	}
	if controller.youtube != nil {
		available = append(available, controller.youtube.source())
	}
	if len(available) == 0 {
		return nil
	}
	configPath, _ := appConfigPath()
	running, err := startServer(controller.ctx, serverOptions{
		Name: tr("linn.app.name"), Sources: available, Identity: "desktop:" + configPath,
		InterfaceName: controller.interfaceName, Port: 1338, ScanInterval: controller.scanInterval,
		YouTube: controller.youtube,
	})
	if err != nil {
		return err
	}
	controller.running = running
	return nil
}

func (controller *desktopController) close() error {
	if controller.running == nil {
		return nil
	}
	controller.running.Stop()
	err := controller.running.Wait()
	controller.running = nil
	return err
}

func runDesktop(interfaceName string, scanInterval time.Duration, background bool) error {
	if err := ensureDefaultAutostart(); err != nil {
		return err
	}
	application := app.NewWithID("dev.linns-media-server")
	icon := fyne.NewStaticResource("linns-media-server.png", appIconPNG)
	application.SetIcon(icon)
	window := application.NewWindow(tr("linn.app.name"))
	window.SetIcon(icon)
	window.Resize(fyne.NewSize(800, 760))
	window.CenterOnScreen()
	window.SetCloseIntercept(window.Hide)
	config, err := loadAppConfig()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	controller := &desktopController{ctx: ctx, interfaceName: interfaceName, scanInterval: scanInterval}
	cacheDirectory, cacheError := os.UserCacheDir()
	if cacheError == nil {
		controller.youtube, cacheError = newYouTubeBridge(ctx, filepath.Join(cacheDirectory, "FolderDLNA", "YouTube"))
	}
	if cacheError == nil {
		cacheError = controller.youtube.setPlaybackSettings(config.YouTubePlayback)
	}
	startupError := controller.restart(config)
	showError := func(key string, err error) {
		message := tr(key)
		slog.Error(message, "error", err)
		dialog.ShowError(errors.New(message), window)
	}
	youtubePanel, refreshYouTube := newYouTubePanel(ctx, controller.youtube, func() error {
		next := config
		next.YouTubeEnabled = true
		if err := saveAppConfig(next); err != nil {
			return err
		}
		config = next
		return nil
	}, func(settings youtubePlaybackSettings) error {
		next := config
		next.YouTubePlayback = settings
		if err := saveAppConfig(next); err != nil {
			showError("linn.youtube.settings_error", err)
			return err
		}
		config = next
		return nil
	})
	var rebuild, refreshTray func()
	addLibrary := func(folder fyne.ListableURI, err error) {
		if err != nil {
			showError("linn.error.open_folder", err)
			return
		}
		if folder == nil {
			return
		}
		source, err := sourceForPath(folder.Path())
		if err != nil {
			showError("linn.error.add_library", err)
			return
		}
		for _, existing := range config.Libraries {
			if existing.ID == source.ID {
				dialog.ShowInformation(tr("linn.info.library_exists"), source.Path, window)
				return
			}
		}
		next := config
		next.Libraries = append(append([]librarySource(nil), config.Libraries...), source)
		if err := saveAppConfig(next); err != nil {
			showError("linn.error.save_library", err)
			return
		}
		config = next
		if err := controller.restart(config); err != nil {
			showError("linn.error.start_server", err)
		}
		rebuild()
	}
	openLibraryPicker := func() {
		picker := newFolderOpen(addLibrary, window)
		picker.SetTitleText(tr("linn.picker.title"))
		picker.SetConfirmText(tr("linn.picker.add"))
		picker.SetDismissText(tr("linn.picker.cancel"))
		picker.Show()
	}
	rebuild = func() {
		rows := container.NewVBox()
		if len(config.Libraries) == 0 && controller.youtube == nil {
			emptyTitle := widget.NewLabelWithStyle(tr("linn.empty.title"), fyne.TextAlignCenter, fyne.TextStyle{Bold: true})
			emptyText := widget.NewLabelWithStyle(tr("linn.empty.description"), fyne.TextAlignCenter, fyne.TextStyle{})
			rows.Add(container.NewCenter(container.NewVBox(
				container.NewCenter(widget.NewIcon(theme.FolderIcon())),
				emptyTitle,
				emptyText,
			)))
		}
		for _, library := range config.Libraries {
			library := library
			state := tr("linn.library.published")
			if info, err := os.Stat(library.Path); err != nil || !info.IsDir() {
				state = tr("linn.library.unavailable")
			}
			remove := widget.NewButtonWithIcon(tr("linn.action.remove"), theme.DeleteIcon(), func() {
				next := config
				next.Libraries = make([]librarySource, 0, len(config.Libraries)-1)
				for _, candidate := range config.Libraries {
					if candidate.ID != library.ID {
						next.Libraries = append(next.Libraries, candidate)
					}
				}
				if err := saveAppConfig(next); err != nil {
					showError("linn.error.save_libraries", err)
					return
				}
				config = next
				if err := controller.restart(config); err != nil {
					showError("linn.error.restart_server", err)
				}
				rebuild()
			})
			remove.Importance = widget.LowImportance
			stateLabel := widget.NewLabel(state)
			path := widget.NewLabel(library.Path)
			path.TextStyle = fyne.TextStyle{Monospace: true}
			details := container.NewVBox(
				widget.NewLabelWithStyle(library.Name, fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
				path,
				stateLabel,
			)
			libraryIcon := container.NewGridWrap(fyne.NewSize(28, 28), widget.NewIcon(theme.FolderIcon()))
			libraryRow := container.NewBorder(nil, nil, libraryIcon, remove, details)
			rows.Add(widget.NewCard("", "", libraryRow))
		}
		rows.Add(youtubePanel)
		status := tr("linn.server.stopped")
		if controller.running != nil {
			count := len(config.Libraries)
			if controller.youtube != nil {
				count++
			}
			status = fmt.Sprintf(tr("linn.server.running"), count)
		} else if len(config.Libraries) == 0 {
			status = tr("linn.server.add_library")
		}
		logo := widget.NewIcon(icon)
		serverStatus := widget.NewLabel(status)
		headerText := container.NewVBox(
			widget.NewLabelWithStyle(tr("linn.app.name"), fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
			serverStatus,
		)
		header := widget.NewCard("", "", container.NewBorder(nil, nil, container.NewGridWrap(fyne.NewSize(58, 58), logo), nil, headerText))
		add := widget.NewButtonWithIcon(tr("linn.action.add_library"), theme.FolderIcon(), openLibraryPicker)
		add.Importance = widget.HighImportance
		footer := container.NewVBox(widget.NewSeparator(), container.NewPadded(container.NewHBox(add)))
		content := container.NewBorder(header, footer, nil, nil, container.NewVScroll(container.NewPadded(rows)))
		window.SetContent(container.NewPadded(content))
		if refreshTray != nil {
			refreshTray()
		}
	}
	rebuild()
	quit := func() {
		cancel()
		if controller.youtube != nil {
			controller.youtube.refreshMu.Lock()
			controller.youtube.refreshMu.Unlock()
			controller.youtube.resolveMu.Lock()
			controller.youtube.resolveMu.Unlock()
			controller.youtube.searchSlot <- struct{}{}
			<-controller.youtube.searchSlot
		}
		_ = controller.close()
		application.Quit()
	}
	if desktopApp, ok := application.(desktop.App); ok {
		desktopApp.SetSystemTrayIcon(icon)
		refreshTray = func() {
			running := controller.running != nil
			statusText := tr("linn.tray.status_stopped")
			if running {
				statusText = tr("linn.tray.status_running")
			}
			status := fyne.NewMenuItem(statusText, nil)
			status.Disabled = true

			toggleText := tr("linn.action.start_server")
			toggle := fyne.NewMenuItem(toggleText, func() {
				if err := controller.restart(config); err != nil {
					showError("linn.error.start_server", err)
				}
				rebuild()
			})
			if running {
				toggle.Label = tr("linn.action.stop_server")
				toggle.Action = func() {
					if err := controller.close(); err != nil {
						showError("linn.error.stop_server", err)
					}
					rebuild()
				}
			}

			restart := fyne.NewMenuItem(tr("linn.action.restart_server"), func() {
				if err := controller.restart(config); err != nil {
					showError("linn.error.restart_server", err)
				}
				rebuild()
			})
			restart.Disabled = !running

			scan := fyne.NewMenuItem(tr("linn.action.scan_libraries"), func() {
				if err := controller.restart(config); err != nil {
					showError("linn.error.scan_libraries", err)
				}
				rebuild()
			})
			scan.Disabled = !running

			libraryItems := make([]*fyne.MenuItem, 0, len(config.Libraries))
			for _, library := range config.Libraries {
				item := fyne.NewMenuItem(library.Name, nil)
				item.Disabled = true
				libraryItems = append(libraryItems, item)
			}
			count := len(libraryItems)
			if controller.youtube != nil {
				item := fyne.NewMenuItem("YouTube", nil)
				item.Disabled = true
				libraryItems = append(libraryItems, item)
				count++
			}
			if len(libraryItems) == 0 {
				item := fyne.NewMenuItem(tr("linn.libraries.none"), nil)
				item.Disabled = true
				libraryItems = append(libraryItems, item)
			}
			libraries := fyne.NewMenuItem(fmt.Sprintf(tr("linn.libraries.count"), count), nil)
			libraries.ChildMenu = fyne.NewMenu(tr("linn.libraries.title"), libraryItems...)
			autostart, autostartError := autostartEnabled()
			autostartItem := fyne.NewMenuItem(tr("linn.autostart.login"), func() {
				if err := setAutostart(!autostart); err != nil {
					showError("linn.error.autostart", err)
					return
				}
				rebuild()
			})
			autostartItem.Checked = autostart
			autostartItem.Disabled = autostartError != nil

			desktopApp.SetSystemTrayMenu(fyne.NewMenu(tr("linn.app.name"),
				status,
				fyne.NewMenuItemSeparator(),
				toggle,
				restart,
				scan,
				fyne.NewMenuItemSeparator(),
				fyne.NewMenuItem(tr("linn.action.add_library"), openLibraryPicker),
				fyne.NewMenuItem("YouTube", window.Show),
				libraries,
				autostartItem,
				fyne.NewMenuItemSeparator(),
				fyne.NewMenuItem(fmt.Sprintf(tr("linn.action.open_app"), tr("linn.app.name")), window.Show),
				fyne.NewMenuItem(tr("linn.action.quit"), quit),
			))
		}
		refreshTray()
	}
	if !background || initialLibraryRequestPending() {
		window.Show()
	}
	if startupError != nil {
		showError("linn.error.start_server", startupError)
	}
	if cacheError != nil {
		showError("linn.youtube.cache_error", cacheError)
	}
	go func() {
		time.Sleep(250 * time.Millisecond)
		fyne.Do(func() {
			if err := requestInitialLibraries(); err != nil {
				showError("linn.error.folder_access", err)
				return
			}
			next, err := loadAppConfig()
			if err != nil {
				showError("linn.error.load_libraries", err)
				return
			}
			config = next
			if err := controller.restart(config); err != nil {
				showError("linn.error.start_server", err)
			}
			rebuild()
		})
	}()
	go func() {
		time.Sleep(500 * time.Millisecond)
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for {
			fyne.Do(func() {
				if config.YouTubeEnabled {
					refreshYouTube()
				}
			})
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	application.Run()
	return controller.close()
}

func newYouTubePanel(ctx context.Context, bridge *youtubeBridge, connected func() error, savePlayback func(youtubePlaybackSettings) error) (fyne.CanvasObject, func()) {
	status := widget.NewLabel(tr("linn.youtube.hint"))
	status.Wrapping = fyne.TextWrapWord
	progress := widget.NewProgressBar()
	progress.Max = float64(len(youtubeFeeds) + 1)
	progress.Hide()
	var refresh, connect *widget.Button
	var cancelRefresh context.CancelFunc
	cancelButton := widget.NewButton(tr("linn.youtube.cancel"), func() {
		if cancelRefresh != nil {
			cancelRefresh()
		}
	})
	cancelButton.Hide()
	update := func(login bool) {
		if bridge == nil || cancelRefresh != nil {
			return
		}
		if !login && !bridge.hasSession() {
			return
		}
		connect.Disable()
		refresh.Disable()
		progress.SetValue(0)
		progress.Show()
		cancelButton.Show()
		status.SetText(tr("linn.youtube.refreshing"))
		operationCtx, cancel := context.WithCancel(ctx)
		cancelRefresh = cancel
		if login {
			status.SetText(tr("linn.youtube.waiting_browser"))
		}
		go func() {
			defer cancel()
			var err error
			if login {
				var directory string
				directory, err = installYouTubeExtension()
				if err == nil {
					loginCtx, loginCancel := context.WithTimeout(operationCtx, 10*time.Minute)
					err = bridge.connectBrowser(loginCtx, directory, func(u *url.URL) error {
						if runtime.GOOS == "darwin" {
							return exec.CommandContext(loginCtx, "/usr/bin/open", "-a", "/Applications/Yandex.app", u.String()).Run()
						}
						return fyne.CurrentApp().OpenURL(u)
					})
					loginCancel()
				}
			}
			if err == nil {
				refreshCtx, refreshCancel := context.WithTimeout(operationCtx, 4*time.Minute)
				err = bridge.refresh(refreshCtx, func(completed int, key string) {
					fyne.Do(func() {
						progress.SetValue(float64(completed))
						if key != "" {
							status.SetText(fmt.Sprintf(tr("linn.youtube.loading_feed"), completed, len(youtubeFeeds), tr(key)))
						}
					})
				})
				refreshCancel()
			}
			fyne.Do(func() {
				progress.Hide()
				cancelButton.Hide()
				cancelRefresh = nil
				connect.Enable()
				if bridge.hasSession() {
					refresh.Enable()
				}
				if errors.Is(err, context.Canceled) {
					status.SetText(tr("linn.youtube.canceled"))
					return
				}
				if errors.Is(err, context.DeadlineExceeded) {
					status.SetText(tr("linn.youtube.timeout"))
					return
				}
				if bridge.hasSession() {
					if saveErr := connected(); saveErr != nil {
						status.SetText(tr("linn.youtube.save_error"))
						return
					}
				}
				if err != nil {
					key := "linn.youtube.account_error"
					if strings.HasPrefix(err.Error(), "linn.youtube.feed_error:") {
						key = "linn.youtube.feed_error"
					}
					slog.Error("YouTube connection or refresh failed", "error", err)
					status.SetText(tr(key))
					return
				}
				status.SetText(tr("linn.youtube.ready"))
			})
		}()
	}
	connect = widget.NewButton(tr("linn.youtube.connect"), func() { update(true) })
	refresh = widget.NewButton(tr("linn.youtube.refresh"), func() { update(false) })
	if bridge == nil {
		connect.Disable()
		refresh.Disable()
		status.SetText(tr("linn.youtube.cache_error"))
	} else if !bridge.hasSession() {
		refresh.Disable()
	}
	content := container.NewVBox(status, container.NewGridWithColumns(2, connect, refresh), progress, cancelButton)
	if bridge != nil {
		settings := bridge.playbackSettings()
		save := func(next youtubePlaybackSettings) error {
			if err := savePlayback(next); err != nil {
				return err
			}
			if err := bridge.setPlaybackSettings(next); err != nil {
				return err
			}
			entries, _ := bridge.entries(bridge.source().ID+"/recommended", false)
			bridge.prewarm(entries)
			return nil
		}
		codecNames := map[string]string{"hevc": "HEVC (H.265)", "h264": "H.264"}
		codecs := []string{codecNames[settings.Codecs[0]], codecNames[settings.Codecs[1]]}
		codecPicker := newYouTubeOrderPicker([]string{"HEVC (H.265)", "H.264"}, codecs, func(order []string) error {
			next := bridge.playbackSettings()
			for i, name := range order {
				for codec, label := range codecNames {
					if label == name {
						next.Codecs[i] = codec
					}
				}
			}
			return save(next)
		})
		var resolutions []string
		for _, resolution := range settings.Resolutions {
			resolutions = append(resolutions, strconv.Itoa(resolution)+"p")
		}
		resolutionPicker := newYouTubeOrderPicker([]string{"2160p", "1440p", "1080p", "720p", "480p"}, resolutions, func(order []string) error {
			next := bridge.playbackSettings()
			for i, resolution := range order {
				next.Resolutions[i], _ = strconv.Atoi(strings.TrimSuffix(resolution, "p"))
			}
			return save(next)
		})
		hint := widget.NewLabel(tr("linn.youtube.playback_hint"))
		hint.Wrapping = fyne.TextWrapWord
		content.Add(widget.NewSeparator())
		content.Add(widget.NewLabel(tr("linn.youtube.codec_order")))
		content.Add(codecPicker)
		content.Add(widget.NewLabel(tr("linn.youtube.resolution_order")))
		content.Add(resolutionPicker)
		content.Add(hint)
		searchStatus := widget.NewLabel(bridge.currentSearchStatus())
		searchStatus.Wrapping = fyne.TextWrapWord
		content.Add(searchStatus)
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			previous := ""
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
				current := bridge.currentSearchStatus()
				if current != previous {
					previous = current
					fyne.Do(func() { searchStatus.SetText(current) })
				}
			}
		}()
	}
	panel := widget.NewCard("YouTube", "", content)
	return panel, func() { update(false) }
}

func newYouTubeOrderPicker(options, order []string, changed func([]string) error) *fyne.Container {
	order = slices.Clone(order)
	row := container.NewGridWithColumns(len(order))
	pickers := make([]*widget.Select, len(order))
	updating := false
	for i, value := range order {
		picker := widget.NewSelect(options, nil)
		picker.SetSelected(value)
		picker.OnChanged = func(value string) {
			if updating {
				return
			}
			other := slices.Index(order, value)
			if other < 0 || other == i {
				return
			}
			next := slices.Clone(order)
			next[i], next[other] = next[other], next[i]
			if changed(next) == nil {
				order = next
			}
			updating = true
			for j, control := range pickers {
				control.SetSelected(order[j])
			}
			updating = false
		}
		pickers[i] = picker
		row.Add(container.NewBorder(nil, nil, widget.NewLabel(strconv.Itoa(i+1)), nil, picker))
	}
	return row
}

//go:embed Icon.png
var appIconPNG []byte
