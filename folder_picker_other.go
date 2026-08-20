//go:build !darwin

package main

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/dialog"
)

func newFolderOpen(callback func(fyne.ListableURI, error), parent fyne.Window) *dialog.FileDialog {
	return dialog.NewFolderOpen(callback, parent)
}

func requestInitialLibraries() error {
	return nil
}

func initialLibraryRequestPending() bool {
	return false
}
