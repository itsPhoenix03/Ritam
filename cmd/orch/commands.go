package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"projectorch/internal/backup"
	"projectorch/internal/brand"
	"projectorch/internal/layout"
	"projectorch/internal/storage"
)

// Commands for managing where the app keeps its data. They run without the
// TUI and, except for where and backups list, need orch closed in the
// project, since they replace or read its database directly.

// commandUsage lists the commands.
func commandUsage() string {
	n := brand.Name
	return strings.Join([]string{
		"",
		"Data commands (run in the project folder):",
		"  " + n + " where                      show where this project's data and backups are",
		"  " + n + " backups [list]             list this project's backups",
		"  " + n + " backups now                take a backup now",
		"  " + n + " backups projects           list every known project and its backups",
		"  " + n + " backups delete <project>   delete a project's backups (and forget it if it is gone)",
		"  " + n + " backups keep <project>     keep a deleted project's backups without asking again",
		"  " + n + " restore [latest|<file>]    restore this project from a backup or an archive",
		"  " + n + " archive [-clear]           archive this project's tasks; -clear then starts it fresh",
		"  " + n + " rename -from <old name>    rename folders made by a version with another name",
	}, "\n")
}

// runCommand runs the data command named by args[0], if it is one.
func runCommand(args []string, out io.Writer) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case "where":
		return true, cmdWhere(args[1:], out)
	case "backups":
		return true, cmdBackups(args[1:], out)
	case "restore":
		return true, cmdRestore(args[1:], out)
	case "archive":
		return true, cmdArchive(args[1:], out)
	case "rename":
		return true, cmdRename(args[1:], out)
	}
	return false, nil
}

// commandFlags parses a command's flags, including -config.
func commandFlags(name string, args []string, extra func(fs *flag.FlagSet)) (*flag.FlagSet, string, error) {
	fs := flag.NewFlagSet(brand.Name+" "+name, flag.ContinueOnError)
	configPath := fs.String("config", "", "the project's settings file (its folder is the project)")
	if extra != nil {
		extra(fs)
	}
	if err := fs.Parse(args); err != nil {
		return nil, "", err
	}
	return fs, *configPath, nil
}

func cmdWhere(args []string, out io.Writer) error {
	_, cfgPath, err := commandFlags("where", args, nil)
	if err != nil {
		return err
	}
	cfg, pl, err := prepare(cfgPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "app home:        %s\n", pl.home.Root)
	fmt.Fprintf(out, "global settings: %s\n", pl.home.ConfigPath())
	fmt.Fprintf(out, "project:         %s (id %s)\n", pl.proj.Root, pl.proj.ID)
	fmt.Fprintf(out, "project folder:  %s\n", pl.proj.Dir)
	fmt.Fprintf(out, "project settings:%s\n", " "+pl.proj.ConfigPath())
	fmt.Fprintf(out, "database:        %s\n", cfg.Storage.Path)
	fmt.Fprintf(out, "log:             %s\n", cfg.Logging.File)
	fmt.Fprintf(out, "backups:         %s\n", pl.home.BackupDir(pl.proj))
	printNotices(out, pl.notices)
	return nil
}

func cmdBackups(args []string, out io.Writer) error {
	fs, cfgPath, err := commandFlags("backups", args, nil)
	if err != nil {
		return err
	}
	cfg, pl, err := prepare(cfgPath)
	if err != nil {
		return err
	}
	rest := fs.Args()
	sub := "list"
	if len(rest) > 0 {
		sub = rest[0]
	}
	switch sub {
	case "list":
		list, err := backup.List(pl.home.BackupDir(pl.proj))
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Fprintf(out, "no backups of %s yet (%s)\n", pl.proj.Name, pl.home.BackupDir(pl.proj))
			return nil
		}
		fmt.Fprintf(out, "backups of %s, newest first (%s):\n", pl.proj.Name, pl.home.BackupDir(pl.proj))
		for _, s := range list {
			fmt.Fprintf(out, "  %s  %-16s %8s  %s\n", s.Taken.Format("2006-01-02 15:04:05"), s.Reason, size(s.Size), filepath.Base(s.Path))
		}
	case "now":
		db, err := storage.OpenSQLite(cfg.Storage.Path)
		if err != nil {
			return closeOrchFirst(err)
		}
		defer db.Close()
		path, err := backup.Take(context.Background(), db, pl.home.BackupDir(pl.proj), "manual")
		if err != nil {
			return err
		}
		_ = backup.Prune(pl.home.BackupDir(pl.proj), cfg.Backups.Keep)
		fmt.Fprintf(out, "backup taken: %s\n", path)
	case "projects":
		for _, e := range pl.reg.Entries() {
			list, _ := backup.List(filepath.Join(pl.home.BackupsDir(), e.Key()))
			state := "last opened " + e.LastSeen.Format("2006-01-02")
			if !e.MissingSince.IsZero() {
				state = "GONE since " + e.MissingSince.Format("2006-01-02")
				if e.Keep {
					state += " (backups kept)"
				}
			}
			fmt.Fprintf(out, "  %-28s %3d backups  %s  %s\n", e.Key(), len(list), state, e.Path)
		}
	case "delete", "keep":
		if len(rest) < 2 {
			return fmt.Errorf("usage: %s backups %s <project>", brand.Name, sub)
		}
		e, ok := pl.reg.Find(rest[1])
		if !ok {
			return fmt.Errorf("no known project %q (see `%s backups projects`)", rest[1], brand.Name)
		}
		if sub == "keep" {
			if err := pl.reg.SetKeep(e.ID); err != nil {
				return err
			}
			fmt.Fprintf(out, "keeping the backups of %s\n", e.Key())
			return nil
		}
		if e.ID == pl.proj.ID {
			return errors.New("that is this project; run the command from somewhere else, or use `archive -clear`")
		}
		if err := os.RemoveAll(filepath.Join(pl.home.BackupsDir(), e.Key())); err != nil {
			return err
		}
		if !e.MissingSince.IsZero() {
			_ = pl.reg.Remove(e.ID)
		}
		fmt.Fprintf(out, "deleted the backups of %s\n", e.Key())
	default:
		return fmt.Errorf("unknown backups command %q%s", sub, commandUsage())
	}
	return nil
}

func cmdRestore(args []string, out io.Writer) error {
	fs, cfgPath, err := commandFlags("restore", args, nil)
	if err != nil {
		return err
	}
	cfg, pl, err := prepare(cfgPath)
	if err != nil {
		return err
	}
	dir := pl.home.BackupDir(pl.proj)
	list, err := backup.List(dir)
	if err != nil {
		return err
	}
	if fs.NArg() == 0 {
		if len(list) == 0 {
			fmt.Fprintf(out, "no backups of %s (%s)\n", pl.proj.Name, dir)
			return nil
		}
		fmt.Fprintf(out, "pick one: `%s restore latest` or `%s restore <file>`\n", brand.Name, brand.Name)
		for _, s := range list {
			fmt.Fprintf(out, "  %s  %-16s  %s\n", s.Taken.Format("2006-01-02 15:04:05"), s.Reason, filepath.Base(s.Path))
		}
		return nil
	}
	src := fs.Arg(0)
	switch {
	case src == "latest":
		if len(list) == 0 {
			return fmt.Errorf("no backups of %s to restore", pl.proj.Name)
		}
		src = list[0].Path
	case !filepath.IsAbs(src) && !fileExists(src):
		src = filepath.Join(dir, src)
	}
	if !fileExists(src) {
		return fmt.Errorf("no such backup: %s", src)
	}

	// What is about to be replaced is backed up first, so a restore can be
	// undone. Opening the database also proves orch is not using it.
	db, err := storage.OpenSQLite(cfg.Storage.Path)
	if err != nil {
		return closeOrchFirst(err)
	}
	if n, _ := countTasks(db); n > 0 {
		if p, err := backup.Take(context.Background(), db, dir, "before-restore"); err == nil {
			fmt.Fprintf(out, "kept the current data as %s\n", filepath.Base(p))
		}
	}
	_ = db.Close()
	if strings.HasSuffix(src, ".zip") {
		err = backup.RestoreArchive(src, cfg.Storage.Path)
	} else {
		err = backup.Restore(src, cfg.Storage.Path)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "restored %s from %s\n", pl.proj.Name, filepath.Base(src))
	return nil
}

func cmdArchive(args []string, out io.Writer) error {
	var clear bool
	_, cfgPath, err := commandFlags("archive", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&clear, "clear", false, "after archiving, clear the project's tasks, team and sessions")
	})
	if err != nil {
		return err
	}
	cfg, pl, err := prepare(cfgPath)
	if err != nil {
		return err
	}
	db, err := storage.OpenSQLite(cfg.Storage.Path)
	if err != nil {
		return closeOrchFirst(err)
	}
	n, _ := countTasks(db)
	zip := filepath.Join(pl.home.ArchivesDir(), pl.proj.Key(), time.Now().Format("20060102-150405")+".zip")
	err = backup.Archive(context.Background(), db, []string{pl.proj.TeamFile(), cfg.Sessions.File}, zip)
	_ = db.Close()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "archived %d tasks of %s to %s\n", n, pl.proj.Name, zip)
	if !clear {
		return nil
	}
	for _, f := range []string{cfg.Storage.Path, cfg.Storage.Path + "-wal", cfg.Storage.Path + "-shm",
		pl.proj.TeamFile(), cfg.Sessions.File} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("clear: %w", err)
		}
	}
	fmt.Fprintf(out, "cleared %s: it starts fresh next time (`%s restore %s` brings the tasks back)\n",
		pl.proj.Name, brand.Name, zip)
	return nil
}

func cmdRename(args []string, out io.Writer) error {
	var from string
	_, _, err := commandFlags("rename", args, func(fs *flag.FlagSet) {
		fs.StringVar(&from, "from", "", "the name the folders were made under")
	})
	if err != nil {
		return err
	}
	if from == "" {
		return fmt.Errorf("usage: %s rename -from <old name>", brand.Name)
	}
	msg, err := layout.RenameHome(from)
	if err != nil {
		return err
	}
	if msg != "" {
		fmt.Fprintln(out, msg)
	}
	home, err := layout.ResolveHome()
	if err != nil {
		return err
	}
	reg, err := layout.LoadRegistry(home.ProjectsFile())
	if err != nil {
		return err
	}
	wd, _ := os.Getwd()
	done, err := layout.RenameAll(from, reg, wd)
	for _, d := range done {
		fmt.Fprintln(out, d)
	}
	if msg == "" && len(done) == 0 && err == nil {
		fmt.Fprintf(out, "nothing named %q found to rename\n", from)
	}
	return err
}

func countTasks(db *storage.SQLite) (int, error) {
	tasks, err := db.Storage().Tasks.List(context.Background())
	return len(tasks), err
}

func closeOrchFirst(err error) error {
	var locked *storage.ErrLocked
	if errors.As(err, &locked) {
		return fmt.Errorf("%s is running in this project; quit it first (%v)", brand.Name, err)
	}
	return err
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func size(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func printNotices(out io.Writer, notices []string) {
	for _, n := range notices {
		fmt.Fprintln(out, "note: "+n)
	}
}
