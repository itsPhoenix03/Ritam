package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"ritam/internal/backup"
	"ritam/internal/brand"
	"ritam/internal/config"
	"ritam/internal/layout"
)

// place is where this run keeps its files: the app home, the record of known
// projects, and the project ritam was started in.
type place struct {
	home layout.Home
	reg  *layout.Registry
	proj layout.Project
	// notices are things the human should know at start: a restore that is
	// possible, a deleted project's backups, folders renamed.
	notices []string
}

// prepare finds or creates the app home and the project's hidden folder,
// loads the settings (defaults, then the app home's, then the project's),
// and points every file the run uses at the right place.
//
// configPath is the -config flag: a settings file for the project, whose
// folder is then the project. Empty means the folder ritam was started in,
// with its settings in <project>/<name>.yaml (if any) and
// <project>/.<name>/<name>.yaml.
func prepare(configPath string) (*config.Config, place, error) {
	var pl place
	for _, old := range brand.FormerNames {
		msg, err := layout.RenameHome(old)
		if err != nil {
			return nil, pl, err
		}
		if msg != "" {
			pl.notices = append(pl.notices, msg)
		}
	}
	home, err := layout.ResolveHome()
	if err != nil {
		return nil, pl, err
	}
	pl.home = home
	if pl.reg, err = layout.LoadRegistry(home.ProjectsFile()); err != nil {
		return nil, pl, err
	}

	root, err := os.Getwd()
	if err != nil {
		return nil, pl, err
	}
	visible := filepath.Join(root, brand.ConfigFile())
	if configPath != "" {
		if visible, err = filepath.Abs(configPath); err != nil {
			return nil, pl, err
		}
		root = filepath.Dir(visible)
	}
	for _, old := range brand.FormerNames {
		done, err := layout.RenameAll(old, pl.reg, root)
		if err != nil {
			return nil, pl, err
		}
		pl.notices = append(pl.notices, done...)
	}

	// The workspace decides the project: normally the folder ritam started
	// in; after a global launch, ritam's own folder in the app home.
	cfg, err := config.LoadLayers(home.ConfigPath(), visible)
	if err != nil {
		return nil, pl, err
	}
	ws, err := resolveWorkspace(cfg, root, home)
	if err != nil {
		return nil, pl, err
	}
	projectRoot := ws.Root
	if ws.Global {
		projectRoot = home.OwnWorkspace()
		if err := os.MkdirAll(projectRoot, 0o700); err != nil {
			return nil, pl, err
		}
	}
	if pl.proj, err = layout.OpenProject(projectRoot, pl.reg); err != nil {
		return nil, pl, err
	}
	if err := pl.reg.Touch(pl.proj); err != nil {
		return nil, pl, err
	}

	// Now with the project's own settings on top.
	if cfg, err = config.LoadLayers(home.ConfigPath(), visible, pl.proj.ConfigPath()); err != nil {
		return nil, pl, err
	}
	if _, err := resolveWorkspace(cfg, root, home); err != nil {
		return nil, pl, err
	}
	pl.place(cfg, root)
	pl.adoptOldModelsFile()
	pl.notices = append(pl.notices, pl.offerRestore()...)
	pl.notices = append(pl.notices, pl.handleMissing(cfg.Backups)...)
	return cfg, pl, nil
}

// resolveWorkspace settles the workspace's root (relative to the project)
// and ritam's own folder, and resolves it.
func resolveWorkspace(cfg *config.Config, root string, home layout.Home) (config.Workspace, error) {
	switch {
	case cfg.Workspace.Root == "":
		cfg.Workspace.Root = root
	case !filepath.IsAbs(cfg.Workspace.Root):
		cfg.Workspace.Root = filepath.Join(root, cfg.Workspace.Root)
	}
	cfg.Workspace.OwnDir = home.OwnWorkspace()
	return config.ResolveWorkspace(cfg.Workspace)
}

// place points the run's files at the app home and the project. A path a
// settings file gives explicitly is kept, relative to the project.
func (pl *place) place(cfg *config.Config, root string) {
	h, p := pl.home, pl.proj
	cfg.Runtime = config.Runtime{
		HomeDir:      h.Root,
		ModelsFile:   h.ModelsFile(),
		OwnWorkspace: h.OwnWorkspace(),
		ProjectID:    p.ID,
		ProjectName:  p.Name,
		BackupDir:    h.BackupDir(p),
		ScratchDir:   p.TmpDir(),
		TeamFile:     p.TeamFile(),
	}
	set := func(field *string, fallback string) {
		switch {
		case *field == "":
			*field = fallback
		case !filepath.IsAbs(*field):
			*field = filepath.Join(root, *field)
		}
	}
	set(&cfg.Storage.Path, p.DBPath())
	set(&cfg.Sessions.File, p.SessionsFile())
	set(&cfg.Logging.File, h.LogPath(p))
}

// adoptOldModelsFile moves what an older version learned about models from
// the project's hidden folder to the app home, where it now belongs.
func (pl *place) adoptOldModelsFile() {
	old := filepath.Join(pl.proj.Dir, "models.json")
	if _, err := os.Stat(old); err != nil {
		return
	}
	if _, err := os.Stat(pl.home.ModelsFile()); errors.Is(err, os.ErrNotExist) {
		_ = os.Rename(old, pl.home.ModelsFile())
	}
}

// offerRestore notices a project whose hidden folder was just recreated
// while backups of it exist: its data was probably deleted by accident.
func (pl *place) offerRestore() []string {
	if !pl.proj.Created {
		return nil
	}
	list, _ := backup.List(pl.home.BackupDir(pl.proj))
	if len(list) == 0 {
		return nil
	}
	return []string{fmt.Sprintf("this project's %s folder was missing; a backup from %s exists. "+
		"Quit and run `%s restore latest` to bring its tasks back.",
		brand.HiddenDir(), list[0].Taken.Format("2006-01-02 15:04"), brand.Name)}
}

// handleMissing applies the backups setting to projects whose folder is
// gone: "ask" says so (until the human keeps or deletes them), "keep" stays
// quiet, "delete" removes their backups once they have been gone long
// enough.
func (pl *place) handleMissing(bc config.BackupConfig) []string {
	var notices []string
	for _, e := range pl.reg.Missing() {
		if e.ID == pl.proj.ID || e.Keep {
			continue
		}
		dir := filepath.Join(pl.home.BackupsDir(), e.Key())
		switch bc.WhenProjectDeleted {
		case "keep":
		case "delete":
			if time.Since(e.MissingSince) >= time.Duration(bc.DeleteAfterDays)*24*time.Hour {
				if err := os.RemoveAll(dir); err == nil {
					_ = pl.reg.Remove(e.ID)
					notices = append(notices, fmt.Sprintf("deleted the backups of %s (%s), gone since %s",
						e.Name, e.Path, e.MissingSince.Format("2006-01-02")))
				}
			}
		default: // ask
			notices = append(notices, fmt.Sprintf("project %s (%s) no longer exists; its backups are kept. "+
				"`%s backups delete %s` removes them, `%s backups keep %s` stops this message.",
				e.Name, e.Path, brand.Name, e.Key(), brand.Name, e.Key()))
		}
	}
	return notices
}
