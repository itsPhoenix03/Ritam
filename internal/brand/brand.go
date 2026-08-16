// Package brand holds the application's name and everything derived from
// it: the hidden folders, the config and database file names, and the
// environment variable that moves the app home. The CLI's final name is not
// settled, so nothing else spells it out; renaming the app is a change to
// Name here (with the old name added to FormerNames, so existing folders
// are renamed on the next start).
package brand

import "strings"

// Name is the application's name, as typed on the command line.
const Name = "orch"

// FormerNames are names the app was released under before. On start, a
// folder or file still carrying one of them is renamed to Name.
var FormerNames = []string{}

// Title is the name as the TUI shows it.
func Title() string { return strings.ToUpper(Name) }

// HiddenDir is the per-project folder: ".orch" in the project root.
func HiddenDir() string { return HiddenDirFor(Name) }

// HiddenDirFor is HiddenDir for another name.
func HiddenDirFor(name string) string { return "." + name }

// ConfigFile is the settings file name, both in the app home and in a
// project's hidden folder.
func ConfigFile() string { return ConfigFileFor(Name) }

// ConfigFileFor is ConfigFile for another name.
func ConfigFileFor(name string) string { return name + ".yaml" }

// DBFile is a project's database file name.
func DBFile() string { return DBFileFor(Name) }

// DBFileFor is DBFile for another name.
func DBFileFor(name string) string { return name + ".db" }

// HomeEnv is the environment variable that overrides the app home.
func HomeEnv() string { return HomeEnvFor(Name) }

// HomeEnvFor is HomeEnv for another name.
func HomeEnvFor(name string) string { return strings.ToUpper(name) + "_HOME" }
