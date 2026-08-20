package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCatalogPublishesLibrariesAsRootFolders(t *testing.T) {
	movies := filepath.Join(t.TempDir(), "Movies")
	series := filepath.Join(t.TempDir(), "Series")
	if err := os.Mkdir(movies, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(series, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(movies, "Movie.mp4"), []byte("movie"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(series, "Episode.mkv"), []byte("episode"), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog, err := newCatalog([]librarySource{{ID: "movies", Name: "Фильмы", Path: movies}, {ID: "series", Name: "Сериалы", Path: series}})
	if err != nil {
		t.Fatal(err)
	}
	root, ok := catalog.children(".")
	if !ok || len(root) != 2 || root[0].Name != "Сериалы" || root[1].Name != "Фильмы" {
		t.Fatalf("unexpected catalog root: %#v", root)
	}
	movieEntries, ok := catalog.children("movies")
	if !ok || len(movieEntries) != 1 || movieEntries[0].Name != "Movie.mp4" {
		t.Fatalf("unexpected movie library: %#v", movieEntries)
	}
	file, entry, err := catalog.openMedia(idFor("movies/Movie.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	if entry.Root != movies || entry.SourceRel != "Movie.mp4" {
		t.Fatalf("unexpected media source: %#v", entry)
	}
}
