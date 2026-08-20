package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type appConfig struct {
	Libraries []librarySource `json:"libraries"`
}

func appConfigPath() (string, error) {
	directory, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, "FolderDLNA", "config.json"), nil
}

func loadAppConfig() (appConfig, error) {
	configPath, err := appConfigPath()
	if err != nil {
		return appConfig{}, err
	}
	data, err := os.ReadFile(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return appConfig{}, nil
	}
	if err != nil {
		return appConfig{}, err
	}
	var config appConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return appConfig{}, fmt.Errorf("decode config: %w", err)
	}
	seen := make(map[string]bool)
	for index := range config.Libraries {
		library := &config.Libraries[index]
		abs, err := filepath.Abs(library.Path)
		if err != nil {
			return appConfig{}, err
		}
		library.Path = abs
		if library.ID == "" {
			library.ID = stableUUID(abs)
		}
		if library.Name == "" {
			library.Name = filepath.Base(abs)
		}
		if seen[library.ID] {
			return appConfig{}, fmt.Errorf("duplicate library: %s", library.Path)
		}
		seen[library.ID] = true
	}
	return config, nil
}

func saveAppConfig(config appConfig) error {
	configPath, err := appConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, append(data, '\n'), 0o600)
}

func sourceForPath(folderPath string) (librarySource, error) {
	abs, err := filepath.Abs(folderPath)
	if err != nil {
		return librarySource{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return librarySource{}, err
	}
	if !info.IsDir() {
		return librarySource{}, errors.New("selected path is not a directory")
	}
	return librarySource{ID: stableUUID(abs), Name: filepath.Base(abs), Path: abs}, nil
}
