package main

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	launchersFile       = "launchers.json"
	recentCommandsFile  = "commands-recent.json"
	foldersFile         = "folders.json"
	maxLauncherRecipes  = 50
	maxRecentCommands   = 20
	maxFavouriteFolders = 50
	maxRecentFolders    = 20
	maxRecipeNameRunes  = 64
	maxCommandBytes     = 512
	maxFolderBytes      = 1024
)

var (
	launchersPartRe = regexp.MustCompile(`^\.(launchers|commands-recent|folders)-[0-9a-f]{16}\.part$`)
	recipeIDRe      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

	errInvalidRecipe    = &codedError{"invalid_recipe", "invalid launcher recipe", http.StatusBadRequest}
	errTooManyRecipes   = &codedError{"too_many_recipes", "at most 50 launcher recipes may be saved", http.StatusConflict}
	errInvalidFolder    = &codedError{"invalid_folder", "folder must be a valid absolute path", http.StatusBadRequest}
	errInvalidCommand   = &codedError{"invalid_command", "command is invalid or contains control characters", http.StatusBadRequest}
	errLaunchersNoStore = &codedError{"unsupported", "launcher persistence is not configured on this server", http.StatusNotImplemented}
)

// launcherRecipe is a user-defined agent launch preset stored in
// <stateDir>/launchers/launchers.json.
type launcherRecipe struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Harness string   `json:"harness,omitempty"`
	Command string   `json:"command,omitempty"`
	Options []string `json:"options,omitempty"`
}

// foldersState holds favourite and recent folder paths stored in
// <stateDir>/launchers/folders.json.
type foldersState struct {
	Favourites []string `json:"favourites"`
	Recents    []string `json:"recents"`
}

// launchersResponse answers GET/POST/PUT /api/mux/launchers. Folders is
// omitted for view-only clients so host filesystem paths are not disclosed.
type launchersResponse struct {
	Harnesses      []agentHarness   `json:"harnesses"`
	Recipes        []launcherRecipe `json:"recipes"`
	RecentCommands []string         `json:"recentCommands"`
	Folders        *foldersState    `json:"folders,omitempty"`
}

// launchersStore manages <stateDir>/launchers/{launchers.json,commands-recent.json,folders.json}.
type launchersStore struct {
	dir string

	mu             sync.Mutex
	recipes        []launcherRecipe
	recentCommands []string
	folders        foldersState
}

func newLaunchersStore(dir string) (*launchersStore, error) {
	if dir == "" {
		return nil, errors.New("no launchers state dir")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	if err := checkPrivateDir(dir); err != nil {
		return nil, err
	}
	s := &launchersStore{
		dir:            dir,
		recipes:        []launcherRecipe{},
		recentCommands: []string{},
		folders:        foldersState{Favourites: []string{}, Recents: []string{}},
	}
	removeStateParts(s.dir, launchersPartRe)
	if err := s.loadRecipes(); err != nil {
		return nil, err
	}
	if err := s.loadRecentCommands(); err != nil {
		return nil, err
	}
	if err := s.loadFolders(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *launchersStore) loadRecipes() error {
	path := filepath.Join(s.dir, launchersFile)
	data, err := readStateFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", launchersFile, err)
	}
	var raw []launcherRecipe
	if err := json.Unmarshal(data, &raw); err != nil {
		log.Printf("launchers: %s is corrupt, starting empty: %v", launchersFile, err)
		_ = os.Rename(path, path+".bad")
		return nil
	}
	kept := make([]launcherRecipe, 0, min(len(raw), maxLauncherRecipes))
	for _, r := range raw {
		if clean, ok := cleanLauncherRecipe(r); ok {
			kept = append(kept, clean)
			if len(kept) >= maxLauncherRecipes {
				break
			}
		}
	}
	s.recipes = kept
	return nil
}

func (s *launchersStore) loadRecentCommands() error {
	path := filepath.Join(s.dir, recentCommandsFile)
	data, err := readStateFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", recentCommandsFile, err)
	}
	var raw []string
	if err := json.Unmarshal(data, &raw); err != nil {
		log.Printf("launchers: %s is corrupt, starting empty: %v", recentCommandsFile, err)
		_ = os.Rename(path, path+".bad")
		return nil
	}
	s.recentCommands = dedupStrings(raw, validCommandLine, maxRecentCommands)
	return nil
}

func (s *launchersStore) loadFolders() error {
	path := filepath.Join(s.dir, foldersFile)
	data, err := readStateFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", foldersFile, err)
	}
	var raw foldersState
	if err := json.Unmarshal(data, &raw); err != nil {
		log.Printf("launchers: %s is corrupt, starting empty: %v", foldersFile, err)
		_ = os.Rename(path, path+".bad")
		return nil
	}
	s.folders = foldersState{
		Favourites: dedupStrings(raw.Favourites, validFolderPath, maxFavouriteFolders),
		Recents:    dedupStrings(raw.Recents, validFolderPath, maxRecentFolders),
	}
	return nil
}

func validCommandLine(cmd string) bool {
	c := strings.TrimSpace(cmd)
	return c != "" && len(c) <= maxCommandBytes && !invalidTmuxChars.MatchString(c)
}

func validFolderPath(p string) bool {
	p = strings.TrimSpace(p)
	return p != "" && len(p) <= maxFolderBytes && validCwdForm(p, runtime.GOOS == "windows")
}

func dedupStrings(in []string, valid func(string) bool, limit int) []string {
	seen := map[string]bool{}
	out := make([]string, 0, min(len(in), limit))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if !valid(s) || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func newRecipeID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "rcp-" + hex.EncodeToString(b[:])
}

func cleanLauncherRecipe(r launcherRecipe) (launcherRecipe, bool) {
	r.ID = strings.TrimSpace(r.ID)
	if r.ID == "" {
		r.ID = newRecipeID()
	} else if !recipeIDRe.MatchString(r.ID) {
		return launcherRecipe{}, false
	}
	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" || utf8.RuneCountInString(r.Name) > maxRecipeNameRunes || invalidTmuxChars.MatchString(r.Name) {
		return launcherRecipe{}, false
	}
	if r.Harness != "" {
		h := normalizeHarnessName(r.Harness)
		if h == "" {
			return launcherRecipe{}, false
		}
		r.Harness = h
	}
	r.Command = strings.TrimSpace(r.Command)
	if r.Command != "" && !validCommandLine(r.Command) {
		return launcherRecipe{}, false
	}
	if r.Harness == "" && r.Command == "" {
		return launcherRecipe{}, false
	}
	if !validateLaunchOptions(r.Options) {
		return launcherRecipe{}, false
	}
	if len(r.Options) > 0 {
		r.Options = dedupStrings(r.Options, validLaunchOptionFlag, 16)
	}
	return r, true
}

func (s *launchersStore) saveRecipesLocked() error {
	if s.dir == "" {
		return nil
	}
	data, err := json.Marshal(s.recipes)
	if err != nil {
		return err
	}
	return writeStateFile("launchers", s.dir, "launchers", filepath.Join(s.dir, launchersFile), data, false)
}

func (s *launchersStore) saveRecentCommandsLocked() error {
	if s.dir == "" {
		return nil
	}
	data, err := json.Marshal(s.recentCommands)
	if err != nil {
		return err
	}
	return writeStateFile("launchers", s.dir, "commands-recent", filepath.Join(s.dir, recentCommandsFile), data, false)
}

func (s *launchersStore) saveFoldersLocked() error {
	if s.dir == "" {
		return nil
	}
	data, err := json.Marshal(s.folders)
	if err != nil {
		return err
	}
	return writeStateFile("launchers", s.dir, "folders", filepath.Join(s.dir, foldersFile), data, false)
}

// snapshot returns the current launchersResponse, omitting Folders when
// viewOnly is true.
func (s *launchersStore) snapshot(viewOnly bool) launchersResponse {
	res := launchersResponse{
		Harnesses:      listAgentHarnesses(),
		Recipes:        []launcherRecipe{},
		RecentCommands: []string{},
	}
	if s == nil {
		if !viewOnly {
			res.Folders = &foldersState{Favourites: []string{}, Recents: []string{}}
		}
		return res
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res.Recipes = make([]launcherRecipe, len(s.recipes))
	for i, r := range s.recipes {
		r.Options = slices.Clone(r.Options)
		res.Recipes[i] = r
	}
	res.RecentCommands = slices.Clone(s.recentCommands)
	if !viewOnly {
		res.Folders = &foldersState{
			Favourites: slices.Clone(s.folders.Favourites),
			Recents:    slices.Clone(s.folders.Recents),
		}
	}
	return res
}

// recipeByID looks up a custom recipe by its ID.
func (s *launchersStore) recipeByID(id string) (launcherRecipe, bool) {
	if s == nil || id == "" {
		return launcherRecipe{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.recipes {
		if r.ID == id {
			r.Options = slices.Clone(r.Options)
			return r, true
		}
	}
	return launcherRecipe{}, false
}

// recordRecentCommand prepends cmd to commands-recent.json (max 20, MRU).
func (s *launchersStore) recordRecentCommand(cmd string) error {
	if s == nil {
		return nil
	}
	cmd = strings.TrimSpace(cmd)
	if !validCommandLine(cmd) {
		return errInvalidCommand
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.recentCommands
	s.recentCommands = dedupStrings(append([]string{cmd}, s.recentCommands...), validCommandLine, maxRecentCommands)
	if err := s.saveRecentCommandsLocked(); err != nil {
		s.recentCommands = prev
		return err
	}
	return nil
}

// recordRecentFolder prepends dir to folders.json recents (max 20, MRU).
func (s *launchersStore) recordRecentFolder(dir string) error {
	if s == nil {
		return nil
	}
	dir = strings.TrimSpace(dir)
	if !validFolderPath(dir) {
		return errInvalidFolder
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.folders
	s.folders.Recents = dedupStrings(append([]string{dir}, s.folders.Recents...), validFolderPath, maxRecentFolders)
	if err := s.saveFoldersLocked(); err != nil {
		s.folders = prev
		return err
	}
	return nil
}

type launchersUpdateRequest struct {
	Recipes               *[]launcherRecipe `json:"recipes"`
	Recipe                *launcherRecipe   `json:"recipe"`
	DeleteRecipeID        string            `json:"deleteRecipeId"`
	RecentCommand         string            `json:"recentCommand"`
	Folders               *foldersState     `json:"folders"`
	ToggleFavouriteFolder string            `json:"toggleFavouriteFolder"`
	RecentFolder          string            `json:"recentFolder"`
}

func (s *launchersStore) applyUpdate(req launchersUpdateRequest) error {
	if s == nil {
		return errLaunchersNoStore
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if req.Recipes != nil {
		if len(*req.Recipes) > maxLauncherRecipes {
			return errTooManyRecipes
		}
		next := make([]launcherRecipe, 0, len(*req.Recipes))
		seen := map[string]bool{}
		for _, r := range *req.Recipes {
			clean, ok := cleanLauncherRecipe(r)
			if !ok || seen[clean.ID] {
				return errInvalidRecipe
			}
			seen[clean.ID] = true
			next = append(next, clean)
		}
		prev := s.recipes
		s.recipes = next
		if err := s.saveRecipesLocked(); err != nil {
			s.recipes = prev
			return err
		}
	}

	if req.Recipe != nil {
		clean, ok := cleanLauncherRecipe(*req.Recipe)
		if !ok {
			return errInvalidRecipe
		}
		next := slices.Clone(s.recipes)
		idx := slices.IndexFunc(next, func(r launcherRecipe) bool { return r.ID == clean.ID })
		if idx >= 0 {
			next[idx] = clean
		} else {
			if len(next) >= maxLauncherRecipes {
				return errTooManyRecipes
			}
			next = append(next, clean)
		}
		prev := s.recipes
		s.recipes = next
		if err := s.saveRecipesLocked(); err != nil {
			s.recipes = prev
			return err
		}
	}

	if id := strings.TrimSpace(req.DeleteRecipeID); id != "" {
		next := make([]launcherRecipe, 0, len(s.recipes))
		for _, r := range s.recipes {
			if r.ID != id {
				next = append(next, r)
			}
		}
		prev := s.recipes
		s.recipes = next
		if err := s.saveRecipesLocked(); err != nil {
			s.recipes = prev
			return err
		}
	}

	if cmd := strings.TrimSpace(req.RecentCommand); cmd != "" {
		if !validCommandLine(cmd) {
			return errInvalidCommand
		}
		prev := s.recentCommands
		s.recentCommands = dedupStrings(append([]string{cmd}, s.recentCommands...), validCommandLine, maxRecentCommands)
		if err := s.saveRecentCommandsLocked(); err != nil {
			s.recentCommands = prev
			return err
		}
	}

	if req.Folders != nil {
		for _, f := range req.Folders.Favourites {
			if !validFolderPath(f) {
				return errInvalidFolder
			}
		}
		for _, f := range req.Folders.Recents {
			if !validFolderPath(f) {
				return errInvalidFolder
			}
		}
		prev := s.folders
		s.folders = foldersState{
			Favourites: dedupStrings(req.Folders.Favourites, validFolderPath, maxFavouriteFolders),
			Recents:    dedupStrings(req.Folders.Recents, validFolderPath, maxRecentFolders),
		}
		if err := s.saveFoldersLocked(); err != nil {
			s.folders = prev
			return err
		}
	}

	if fav := strings.TrimSpace(req.ToggleFavouriteFolder); fav != "" {
		if !validFolderPath(fav) {
			return errInvalidFolder
		}
		prev := s.folders
		if idx := slices.Index(s.folders.Favourites, fav); idx >= 0 {
			s.folders.Favourites = slices.Delete(slices.Clone(s.folders.Favourites), idx, idx+1)
		} else {
			if len(s.folders.Favourites) >= maxFavouriteFolders {
				return &codedError{"too_many_folders", "at most 50 favourite folders may be saved", http.StatusConflict}
			}
			s.folders.Favourites = append(slices.Clone(s.folders.Favourites), fav)
		}
		if err := s.saveFoldersLocked(); err != nil {
			s.folders = prev
			return err
		}
	}

	if rec := strings.TrimSpace(req.RecentFolder); rec != "" {
		if !validFolderPath(rec) {
			return errInvalidFolder
		}
		prev := s.folders
		s.folders.Recents = dedupStrings(append([]string{rec}, s.folders.Recents...), validFolderPath, maxRecentFolders)
		if err := s.saveFoldersLocked(); err != nil {
			s.folders = prev
			return err
		}
	}

	return nil
}

func registerLaunchersRoutes(mux *http.ServeMux, store *launchersStore, allowed hostAllowlist) {
	mux.HandleFunc("/api/mux/launchers", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if msg := crossSiteRejection(allowed, r); msg != "" {
				jsonError(w, msg, http.StatusForbidden)
				return
			}
			w.Header().Set("X-Content-Type-Options", "nosniff")
			jsonOK(w, store.snapshot(isViewOnly(r)))
		case http.MethodPost, http.MethodPut:
			if !requireWriteRole(w, r) {
				return
			}
			var req launchersUpdateRequest
			if !decodeJSON(w, r, &req) {
				return
			}
			if err := store.applyUpdate(req); err != nil {
				var ce *codedError
				if errors.As(err, &ce) {
					jsonErrorCode(w, ce.code, ce.msg, ce.status)
					return
				}
				log.Printf("launchers: save failed: %v", err)
				jsonError(w, "internal error", http.StatusInternalServerError)
				return
			}
			jsonOK(w, store.snapshot(false))
		default:
			w.Header().Set("Allow", "GET, POST, PUT")
			jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

func formatLaunchedCommand(harness string, args []string) string {
	h, ok := lookupAgentHarness(harness)
	cmd := cmp.Or(h.Command, harness)
	if !ok {
		cmd = harness
	}
	if len(args) == 0 {
		return cmd
	}
	return cmd + " " + strings.Join(args, " ")
}
