//go:build darwin

package main

/*
#cgo LDFLAGS: -framework Cocoa
#cgo CFLAGS: -fblocks
#include <stdlib.h>
char *folderdlna_choose_directories(const char *title, const char *prompt, const char *message);
*/
import "C"

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/storage"
)

type folderOpen struct {
	callback    func(fyne.ListableURI, error)
	titleText   string
	confirmText string
}

func newFolderOpen(callback func(fyne.ListableURI, error), _ fyne.Window) *folderOpen {
	return &folderOpen{callback: callback, titleText: tr("linn.picker.title"), confirmText: tr("linn.picker.add")}
}

func (d *folderOpen) SetTitleText(text string)   { d.titleText = text }
func (d *folderOpen) SetConfirmText(text string) { d.confirmText = text }
func (d *folderOpen) SetDismissText(string)      {}

func (d *folderOpen) Show() {
	paths, err := chooseNativeFolders(d.titleText, d.confirmText, tr("linn.picker.message"))
	if err != nil {
		d.callback(nil, err)
		return
	}
	if len(paths) == 0 {
		d.callback(nil, nil)
		return
	}
	for _, path := range paths {
		folder, err := storage.ListerForURI(storage.NewFileURI(path))
		d.callback(folder, err)
	}
}

func chooseNativeFolders(title, prompt, message string) ([]string, error) {
	cTitle, cPrompt, cMessage := C.CString(title), C.CString(prompt), C.CString(message)
	defer C.free(unsafe.Pointer(cTitle))
	defer C.free(unsafe.Pointer(cPrompt))
	defer C.free(unsafe.Pointer(cMessage))
	raw := C.folderdlna_choose_directories(cTitle, cPrompt, cMessage)
	if raw == nil {
		return nil, errors.New(tr("linn.error.native_folder_dialog"))
	}
	defer C.free(unsafe.Pointer(raw))

	var paths []string
	if err := json.Unmarshal([]byte(C.GoString(raw)), &paths); err != nil {
		return nil, fmt.Errorf("%s: %w", tr("linn.error.decode_folders"), err)
	}
	return paths, nil
}

func requestInitialLibraries() error {
	marker, err := folderAccessMarker()
	if _, err := os.Stat(marker); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}

	config, err := loadAppConfig()
	if err != nil {
		return err
	}
	paths, err := chooseNativeFolders(tr("linn.picker.title"), tr("linn.picker.add"), tr("linn.picker.message"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		if strings.TrimSpace(path) != "" {
			source, err := sourceForPath(path)
			if err != nil {
				return err
			}
			duplicate := false
			for _, existing := range config.Libraries {
				duplicate = duplicate || existing.ID == source.ID
			}
			if !duplicate {
				config.Libraries = append(config.Libraries, source)
			}
		}
	}
	if len(paths) > 0 {
		if err := saveAppConfig(config); err != nil {
			return err
		}
	}

	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		return err
	}
	return os.WriteFile(marker, nil, 0o600)
}

func initialLibraryRequestPending() bool {
	marker, err := folderAccessMarker()
	if err != nil {
		return true
	}
	_, err = os.Stat(marker)
	return errors.Is(err, os.ErrNotExist)
}

func folderAccessMarker() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "FolderDLNA", ".folder-access-requested"), nil
}
