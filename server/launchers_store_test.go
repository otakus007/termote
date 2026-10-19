package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"testing"
)

func sampleAbsFolder(name string) string {
	if runtime.GOOS == "windows" {
		return `C:\projects\` + name
	}
	return "/srv/projects/" + name
}

func TestLaunchersStorePersistenceAndBounds(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "launchers")
	store, err := newLaunchersStore(dir)
	if err != nil {
		t.Fatalf("newLaunchersStore: %v", err)
	}

	// Save a valid recipe
	if err := store.applyUpdate(launchersUpdateRequest{
		Recipe: &launcherRecipe{
			ID:      "claude-fast",
			Name:    "Claude Fast",
			Harness: "claude",
			Options: []string{"--continue", "--dangerously-skip-permissions"},
		},
	}); err != nil {
		t.Fatalf("applyUpdate recipe: %v", err)
	}

	// Reject recipe with control character or bad flag
	if err := store.applyUpdate(launchersUpdateRequest{
		Recipe: &launcherRecipe{
			ID:      "bad",
			Name:    "Bad",
			Harness: "claude",
			Options: []string{"--flag;rm -rf /"},
		},
	}); err == nil {
		t.Fatal("expected invalid option flag in recipe to be rejected")
	}

	// Record 25 recent commands -> capped at 20 MRU
	for i := range 25 {
		if err := store.recordRecentCommand(fmt.Sprintf("claude --continue #%d", i)); err != nil {
			t.Fatalf("recordRecentCommand %d: %v", i, err)
		}
	}
	if err := store.recordRecentCommand("bad\ncommand"); err == nil {
		t.Fatal("expected command with newline to be rejected")
	}

	// Toggle favourite folder and record recent folder
	fav := sampleAbsFolder("herdr")
	rec := sampleAbsFolder("termote")
	if err := store.applyUpdate(launchersUpdateRequest{ToggleFavouriteFolder: fav}); err != nil {
		t.Fatalf("ToggleFavouriteFolder: %v", err)
	}
	if err := store.recordRecentFolder(rec); err != nil {
		t.Fatalf("recordRecentFolder: %v", err)
	}
	if err := store.recordRecentFolder("relative/path"); err == nil {
		t.Fatal("expected relative folder path to be rejected")
	}

	// Reload from disk and verify persisted state
	reloaded, err := newLaunchersStore(dir)
	if err != nil {
		t.Fatalf("reload launchersStore: %v", err)
	}
	snap := reloaded.snapshot(false)
	if len(snap.Recipes) != 1 || snap.Recipes[0].ID != "claude-fast" {
		t.Fatalf("unexpected recipes after reload: %+v", snap.Recipes)
	}
	if len(snap.RecentCommands) != maxRecentCommands || snap.RecentCommands[0] != "claude --continue #24" {
		t.Fatalf("expected %d MRU recent commands starting with #24, got %v", maxRecentCommands, snap.RecentCommands)
	}
	if snap.Folders == nil || len(snap.Folders.Favourites) != 1 || snap.Folders.Favourites[0] != fav {
		t.Fatalf("unexpected favourites after reload: %+v", snap.Folders)
	}
	if len(snap.Folders.Recents) != 1 || snap.Folders.Recents[0] != rec {
		t.Fatalf("unexpected recents after reload: %+v", snap.Folders)
	}

	// View-only snapshot omits folders
	viewSnap := reloaded.snapshot(true)
	if viewSnap.Folders != nil {
		t.Fatalf("expected view-only snapshot to omit Folders, got %+v", viewSnap.Folders)
	}
}

func TestLaunchersRoutesRoleEnforcement(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "launchers")
	store, err := newLaunchersStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	fav := sampleAbsFolder("secret-repo")
	if err := store.recordRecentFolder(fav); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	var allowed hostAllowlist
	registerLaunchersRoutes(mux, store, allowed)

	// 1. Full role GET includes folders
	reqFull := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/mux/launchers", nil)
	recFull := httptest.NewRecorder()
	mux.ServeHTTP(recFull, reqFull)
	if recFull.Code != http.StatusOK {
		t.Fatalf("GET /api/mux/launchers (full) = %d: %s", recFull.Code, recFull.Body.String())
	}
	var fullResp launchersResponse
	if err := json.Unmarshal(recFull.Body.Bytes(), &fullResp); err != nil {
		t.Fatal(err)
	}
	if fullResp.Folders == nil || len(fullResp.Folders.Recents) != 1 {
		t.Fatalf("expected full role GET to include folders, got %+v", fullResp.Folders)
	}

	// 2. View-only role GET omits folders
	reqView := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/mux/launchers", nil)
	reqView = reqView.WithContext(withAuth(reqView.Context(), authInfo{Kind: authDevice, Role: roleView}))
	recView := httptest.NewRecorder()
	mux.ServeHTTP(recView, reqView)
	if recView.Code != http.StatusOK {
		t.Fatalf("GET /api/mux/launchers (view) = %d", recView.Code)
	}
	var viewResp launchersResponse
	if err := json.Unmarshal(recView.Body.Bytes(), &viewResp); err != nil {
		t.Fatal(err)
	}
	if viewResp.Folders != nil {
		t.Fatalf("expected view-only GET to omit folders, got %+v", viewResp.Folders)
	}

	// 3. View-only role POST is rejected with 403 view_only
	body := bytes.NewBufferString(`{"recentCommand":"claude"}`)
	reqPostView := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/mux/launchers", body)
	reqPostView = reqPostView.WithContext(withAuth(reqPostView.Context(), authInfo{Kind: authDevice, Role: roleView}))
	reqPostView.Header.Set("Content-Type", "application/json")
	recPostView := httptest.NewRecorder()
	mux.ServeHTTP(recPostView, reqPostView)
	if recPostView.Code != http.StatusForbidden {
		t.Fatalf("POST /api/mux/launchers (view) = %d, want 403", recPostView.Code)
	}
}
