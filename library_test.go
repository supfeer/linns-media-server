package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLibraryPublishesOnlyFoldersAndVideos(t *testing.T) {
	root := t.TempDir()
	season := filepath.Join(root, "Season 1")
	if err := os.Mkdir(season, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(season, "Episode 1.mkv"), []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(season, "notes.txt"), []byte("text"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".hidden.mp4"), []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}

	library, err := newLibrary(root)
	if err != nil {
		t.Fatal(err)
	}
	rootChildren, ok := library.children(".")
	if !ok || len(rootChildren) != 1 || !rootChildren[0].Dir || rootChildren[0].Name != "Season 1" {
		t.Fatalf("unexpected root children: %#v", rootChildren)
	}
	seasonChildren, ok := library.children("Season 1")
	if !ok || len(seasonChildren) != 1 || seasonChildren[0].Name != "Episode 1.mkv" {
		t.Fatalf("unexpected season children: %#v", seasonChildren)
	}
	if id := idFor("Season 1/Episode 1.mkv"); id == "0" {
		t.Fatal("video object id must not be root")
	} else if rel, err := relForID(id); err != nil || rel != "Season 1/Episode 1.mkv" {
		t.Fatalf("object id round trip failed: rel=%q err=%v", rel, err)
	}
}
