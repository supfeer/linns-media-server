package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
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
	if len(available) == 0 {
		return nil
	}
	configPath, _ := appConfigPath()
	running, err := startServer(controller.ctx, serverOptions{
		Name: tr("linn.app.name"), Sources: available, Identity: "desktop:" + configPath,
		InterfaceName: controller.interfaceName, Port: 1338, ScanInterval: controller.scanInterval,
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

func runDesktop(interfaceName string, scanInterval time.Duration) error {
	if err := ensureDefaultAutostart(); err != nil {
		return err
	}
	application := app.NewWithID("dev.linns-media-server")
	icon := fyne.NewStaticResource("linns-media-server.png", appIconPNG)
	application.SetIcon(icon)
	window := application.NewWindow(tr("linn.app.name"))
	window.SetIcon(icon)
	window.Resize(fyne.NewSize(760, 520))
	window.CenterOnScreen()
	window.SetCloseIntercept(window.Hide)
	config, err := loadAppConfig()
	if err != nil {
		return err
	}
	controller := &desktopController{ctx: context.Background(), interfaceName: interfaceName, scanInterval: scanInterval}
	startupError := controller.restart(config)
	showError := func(key string, err error) {
		message := tr(key)
		slog.Error(message, "error", err)
		dialog.ShowError(errors.New(message), window)
	}
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
		next := appConfig{Libraries: append(append([]librarySource(nil), config.Libraries...), source)}
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
		if len(config.Libraries) == 0 {
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
				next := appConfig{Libraries: make([]librarySource, 0, len(config.Libraries)-1)}
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
		status := tr("linn.server.stopped")
		if controller.running != nil {
			status = fmt.Sprintf(tr("linn.server.running"), len(config.Libraries))
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
	quit := func() { _ = controller.close(); application.Quit() }
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
			scan.Disabled = !running || len(config.Libraries) == 0

			libraryItems := make([]*fyne.MenuItem, 0, len(config.Libraries))
			for _, library := range config.Libraries {
				item := fyne.NewMenuItem(library.Name, nil)
				item.Disabled = true
				libraryItems = append(libraryItems, item)
			}
			if len(libraryItems) == 0 {
				item := fyne.NewMenuItem(tr("linn.libraries.none"), nil)
				item.Disabled = true
				libraryItems = append(libraryItems, item)
			}
			libraries := fyne.NewMenuItem(fmt.Sprintf(tr("linn.libraries.count"), len(config.Libraries)), nil)
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
				libraries,
				autostartItem,
				fyne.NewMenuItemSeparator(),
				fyne.NewMenuItem(fmt.Sprintf(tr("linn.action.open_app"), tr("linn.app.name")), window.Show),
				fyne.NewMenuItem(tr("linn.action.quit"), quit),
			))
		}
		refreshTray()
	}
	if initialLibraryRequestPending() {
		window.Show()
	}
	if startupError != nil {
		showError("linn.error.start_server", startupError)
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
	application.Run()
	return controller.close()
}

//go:embed Icon.png
var appIconPNG []byte
