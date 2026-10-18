package layout

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

// Registry is the app home's record of known projects, kept in
// projects.json. It is how backups are found again after a project's hidden
// folder is deleted, and how a deleted project is noticed.
type Registry struct {
	path string

	mu      sync.Mutex
	entries []Entry
	// forgotten are ids this process removed, which a merge with the file
	// on disk must not bring back.
	forgotten map[string]bool
}

// Entry is one known project.
type Entry struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	LastSeen time.Time `json:"lastSeen"`
	// MissingSince is when the project's folder was first found gone.
	MissingSince time.Time `json:"missingSince,omitzero"`
	// Keep records the human's choice to keep a missing project's backups.
	Keep bool `json:"keep,omitempty"`
	// Access is the access level the human approved for the project on
	// this machine. A project's own settings files travel with it (a clone,
	// a copy), so what they ask for is granted only up to what was approved
	// here, for this project in this folder.
	Access *AccessGrant `json:"access,omitempty"`
}

// AccessGrant is an access level approved for one project folder.
type AccessGrant struct {
	// Path is the folder it was approved for: a copy elsewhere is not
	// covered.
	Path     string    `json:"path"`
	Level    int       `json:"level"`
	Commands []string  `json:"commands,omitempty"`
	At       time.Time `json:"at"`
}

// Grant returns the access approved for project id in folder path.
func (r *Registry) Grant(id, path string) (AccessGrant, bool) {
	if r == nil {
		return AccessGrant{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.entries {
		if e.ID == id && e.Access != nil && samePath(e.Access.Path, path) {
			return *e.Access, true
		}
	}
	return AccessGrant{}, false
}

// SetGrant records the access approved for project id.
func (r *Registry) SetGrant(id string, g AccessGrant) error {
	r.mu.Lock()
	for i := range r.entries {
		if r.entries[i].ID == id {
			g := g
			r.entries[i].Access = &g
		}
	}
	r.mu.Unlock()
	return r.Save()
}

// Key names the project's folders in the app home.
func (e Entry) Key() string { return projectKey(e.Name, e.ID) }

// LoadRegistry reads the registry, empty when it does not exist yet.
func LoadRegistry(path string) (*Registry, error) {
	r := &Registry{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, fmt.Errorf("projects: %w", err)
	}
	if err := json.Unmarshal(data, &r.entries); err != nil {
		return nil, fmt.Errorf("projects: %s: %w", path, err)
	}
	return r, nil
}

// Entries lists the known projects.
func (r *Registry) Entries() []Entry {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Entry(nil), r.entries...)
}

// ByPath finds a project by its folder.
func (r *Registry) ByPath(path string) (Entry, bool) {
	if r == nil {
		return Entry{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.entries {
		if samePath(e.Path, path) {
			return e, true
		}
	}
	return Entry{}, false
}

// Find looks a project up by id, folder name, key or path.
func (r *Registry) Find(what string) (Entry, bool) {
	if r == nil {
		return Entry{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.entries {
		if e.ID == what || e.Key() == what || e.Name == what || samePath(e.Path, what) {
			return e, true
		}
	}
	return Entry{}, false
}

// Touch records that the project was opened now, from where it is now.
func (r *Registry) Touch(p Project) error {
	r.mu.Lock()
	found := false
	for i := range r.entries {
		if r.entries[i].ID == p.ID {
			r.entries[i].Name, r.entries[i].Path = p.Name, p.Root
			r.entries[i].LastSeen, r.entries[i].MissingSince, r.entries[i].Keep = time.Now(), time.Time{}, false
			found = true
		}
	}
	if !found {
		r.entries = append(r.entries, Entry{ID: p.ID, Name: p.Name, Path: p.Root, LastSeen: time.Now()})
	}
	r.mu.Unlock()
	return r.Save()
}

// Missing lists the known projects whose folder no longer exists, noting
// when each was first found gone.
func (r *Registry) Missing() []Entry {
	r.mu.Lock()
	var out []Entry
	changed := false
	for i := range r.entries {
		if _, err := os.Stat(r.entries[i].Path); !errors.Is(err, os.ErrNotExist) {
			continue
		}
		if r.entries[i].MissingSince.IsZero() {
			r.entries[i].MissingSince = time.Now()
			changed = true
		}
		out = append(out, r.entries[i])
	}
	r.mu.Unlock()
	if changed {
		_ = r.Save()
	}
	return out
}

// SetKeep records that a missing project's backups are to be kept.
func (r *Registry) SetKeep(id string) error {
	r.mu.Lock()
	for i := range r.entries {
		if r.entries[i].ID == id {
			r.entries[i].Keep = true
		}
	}
	r.mu.Unlock()
	return r.Save()
}

// Remove forgets a project.
func (r *Registry) Remove(id string) error {
	r.mu.Lock()
	out := r.entries[:0]
	for _, e := range r.entries {
		if e.ID != id {
			out = append(out, e)
		}
	}
	r.entries = out
	if r.forgotten == nil {
		r.forgotten = map[string]bool{}
	}
	r.forgotten[id] = true
	r.mu.Unlock()
	return r.Save()
}

// Save writes the registry. Each process re-reads it before writing and
// merges in its own entries, so two projects starting at once do not erase
// each other's record.
func (r *Registry) Save() error {
	if r == nil || r.path == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	mine := map[string]Entry{}
	for _, e := range r.entries {
		mine[e.ID] = e
	}
	if data, err := os.ReadFile(r.path); err == nil {
		var onDisk []Entry
		if json.Unmarshal(data, &onDisk) == nil {
			for _, e := range onDisk {
				if _, ok := mine[e.ID]; !ok && !r.forgotten[e.ID] {
					mine[e.ID] = e
				}
			}
		}
	}
	list := make([]Entry, 0, len(mine))
	for _, e := range mine {
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name+list[i].ID < list[j].Name+list[j].ID })
	r.entries = list
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(r.path, data)
}
