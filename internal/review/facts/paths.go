package facts

import (
	"path"
	"regexp"
	"strings"
)

// Env is what the parser knows about the machine and the agent: where the
// workspace, the home folder and the temporary folders are, and which
// files are ritam's or protected. Paths are absolute, as the OS writes them;
// either slash will do.
type Env struct {
	// OS is linux, macos or windows. Paths are resolved the way this OS
	// does, whatever machine the parser runs on, so tests can check any.
	OS        string
	Workspace string
	// Build is the agent's build folder, "" when it builds nowhere.
	Build     string
	Home      string
	Temp      []string
	RitamHome string
	// Protected are the files and folders the file guard keeps: absolute,
	// or relative to the workspace; "**" and "*" may be used.
	Protected []string
	// Vars are the well-known environment variables a command may name:
	// HOME, USERPROFILE, APPDATA, LOCALAPPDATA, PROGRAMDATA, WINDIR,
	// PROGRAMFILES, TEMP, TMP, TMPDIR, USER. Others are unknown.
	Vars map[string]string
	// Resolve, if set, resolves symbolic links and junctions on a path's
	// longest existing prefix. A path gets the worse zone of the path as
	// written and as resolved.
	Resolve func(string) string
}

// DefaultProtected is the file guard's list: what decides how things run
// later on the human's machine, relative to the workspace.
var DefaultProtected = []string{
	".git/config", ".git/hooks/**", ".claude/settings.json", ".claude/settings.local.json", ".mcp.json",
	".codex/**", ".vscode/*", ".envrc", "CLAUDE.md", "CLAUDE.local.md", "AGENTS.md", ".github/workflows/**",
	".husky/**",
}

// caseless reports whether the OS compares paths without regard to case.
func (e Env) caseless() bool { return e.OS == "windows" || e.OS == "macos" }

// variable returns a well-known variable's value; "" and false if it is
// not one or not set.
func (e Env) variable(name string) (string, bool) {
	if e.OS == "windows" {
		name = strings.ToUpper(name)
		for k, v := range e.Vars {
			if strings.ToUpper(k) == name && v != "" {
				return slash(v), true
			}
		}
	} else if v := e.Vars[name]; v != "" {
		return slash(v), true
	}
	switch strings.ToUpper(name) {
	case "HOME", "USERPROFILE":
		if e.Home != "" {
			return slash(e.Home), true
		}
	case "TEMP", "TMP", "TMPDIR":
		if len(e.Temp) > 0 {
			return slash(e.Temp[0]), true
		}
	}
	return "", false
}

// slash turns backslashes into forward slashes.
func slash(p string) string { return strings.ReplaceAll(p, `\`, "/") }

var (
	driveRe     = regexp.MustCompile(`^[A-Za-z]:`)
	msysDriveRe = regexp.MustCompile(`^/([A-Za-z])(/|$)`)
	wslDriveRe  = regexp.MustCompile(`^/mnt/([A-Za-z])(/|$)`)
	regKeyRe    = regexp.MustCompile(`(?i)^(?:(?:microsoft\.powershell\.core\\)?registry::)?(hklm|hkcu|hku|hkcr|hkcc|hkey_local_machine|hkey_current_user|hkey_users|hkey_classes_root|hkey_current_config):?(?:[\\/]+(.*))?$`)
	reservedRe  = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[0-9]|lpt[0-9])(\..*)?$`)
)

var hiveNames = map[string]string{
	"hklm": "HKLM", "hkey_local_machine": "HKLM", "hkcu": "HKCU", "hkey_current_user": "HKCU",
	"hku": "HKU", "hkey_users": "HKU", "hkcr": "HKCR", "hkey_classes_root": "HKCR",
	"hkcc": "HKCC", "hkey_current_config": "HKCC",
}

// registryKey reads a registry key as reg.exe or PowerShell names it, in
// the form the rules use: HKLM/Software/..., with WOW6432Node dropped.
func registryKey(s string) (string, bool) {
	m := regKeyRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", false
	}
	key := hiveNames[strings.ToLower(m[1])]
	rest := strings.Trim(slash(m[2]), "/")
	rest = regexp.MustCompile(`(?i)/wow6432node(/|$)`).ReplaceAllString(rest, "$1")
	if rest != "" {
		key += "/" + rest
	}
	return key, true
}

// isAbs reports whether p (forward slashes) is absolute for the OS.
func (e Env) isAbs(p string) bool {
	if e.OS == "windows" {
		return driveRe.MatchString(p) && (len(p) == 2 || p[2] == '/') || strings.HasPrefix(p, "//")
	}
	return strings.HasPrefix(p, "/")
}

// clean tidies an absolute path: no "." or "..", no doubled slashes, a
// capital drive letter, and on Windows no trailing dots or spaces in a
// name, nor an alternate data stream.
func (e Env) clean(p string) string {
	if e.OS != "windows" {
		return path.Clean(p)
	}
	unc := strings.HasPrefix(p, "//")
	if unc {
		p = "/" + strings.TrimLeft(p, "/")
	}
	parts := strings.Split(p, "/")
	for i, s := range parts {
		if i == 0 && driveRe.MatchString(s) {
			parts[i] = strings.ToUpper(s[:1]) + ":"
			continue
		}
		if j := strings.Index(s, ":"); j > 0 {
			s = s[:j] // file.txt:stream is file.txt
		}
		if s != "." && s != ".." {
			s = strings.TrimRight(s, ". ")
		}
		parts[i] = s
	}
	p = strings.Join(parts, "/")
	if driveRe.MatchString(p) {
		rest := path.Clean("/" + p[2:])
		return p[:2] + rest
	}
	p = path.Clean(p)
	if unc {
		p = "/" + p
	}
	return p
}

// join resolves p against the folder dir.
func (e Env) join(dir, p string) string {
	if p == "" {
		return dir
	}
	if e.isAbs(p) {
		return e.clean(p)
	}
	if e.OS == "windows" && strings.HasPrefix(p, "/") && driveRe.MatchString(dir) {
		return e.clean(dir[:2] + p) // \x is on the current drive
	}
	return e.clean(dir + "/" + p)
}

// within reports whether p is root or under it.
func (e Env) within(root, p string) bool {
	if root == "" {
		return false
	}
	root, p = e.clean(slash(root)), e.clean(slash(p))
	if e.caseless() {
		root, p = strings.ToLower(root), strings.ToLower(p)
	}
	return p == root || strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/")
}

// resolved is a path worked out, with what is known of it.
type resolved struct {
	path    string
	zone    Zone
	glob    bool
	dynamic bool
}

// resolve turns a path as written into an absolute path and a zone.
// prefix is the part known before run time when dynamic is set; glob says
// the text has wildcards. dialect matters on Windows, where Git Bash names
// C:\ as /c and its own folders as /usr, /tmp, ...
func (e Env) resolve(text, prefix string, dynamic, glob bool, cwd, dialect string) resolved {
	known := text
	if dynamic {
		known = prefix
	}
	known = slash(known)
	if glob {
		if i := strings.IndexAny(known, "*?["); i >= 0 {
			known = known[:i]
			if j := strings.LastIndex(known, "/"); j >= 0 {
				known = known[:j+1]
			} else {
				known = ""
			}
		}
	}
	r := resolved{glob: glob, dynamic: dynamic}
	if key, ok := registryKey(text); ok && e.OS == "windows" {
		r.path, r.zone = key, ZoneRegistry
		return r
	}
	full, special := e.locate(known, cwd, dialect)
	r.path = full
	switch {
	case special != "":
		r.zone = special
	case cwd == "" && !e.isAbs(full):
		r.zone = ZoneUnknown
	case dynamic:
		// Only a known prefix that already leaves the workspace decides:
		// rm -rf ~/$X is in the home folder whatever $X is.
		z := e.zoneOf(full)
		if prefix == "" || z == ZoneWorkspace || z == ZoneBuild || z == ZoneProtected || z == ZoneRitam {
			z = ZoneUnknown
		}
		r.zone = z
	default:
		r.zone = e.zoneOf(full)
		if e.Resolve != nil && full != "" {
			if real := e.clean(slash(e.Resolve(full))); real != full {
				r.zone = worse(r.zone, e.zoneOf(real))
			}
		}
	}
	if glob && !dynamic && r.zone == ZoneUnknown {
		r.dynamic = true
	}
	return r
}

// locate makes p absolute against cwd. special is a zone decided by the
// form of the path alone: a device, the null device, a network share.
func (e Env) locate(p, cwd, dialect string) (full string, special Zone) {
	if p == "" {
		return e.clean(slash(cwd)), ""
	}
	if e.OS == "windows" {
		lower := strings.ToLower(p)
		switch {
		case strings.HasPrefix(lower, "//?/") || strings.HasPrefix(lower, "//?/unc/"):
			p = p[4:]
			if strings.HasPrefix(strings.ToLower(p), "unc/") {
				p = "//" + p[4:]
			}
		case strings.HasPrefix(lower, "//./"):
			return p, ZoneDevice
		}
		base := p[strings.LastIndex(p, "/")+1:]
		if m := reservedRe.FindStringSubmatch(base); m != nil {
			if n := strings.ToLower(m[1]); n == "nul" || n == "con" {
				return p, ZoneNull
			}
			return p, ZoneDevice
		}
		if dialect == Bash {
			if z, mapped, ok := e.msys(p); ok {
				if z != "" {
					return p, z
				}
				p = mapped
			}
		}
		if strings.HasPrefix(p, "//") {
			return e.clean(p), ZoneUNC
		}
		if m := wslDriveRe.FindStringSubmatch(p); m != nil {
			p = strings.ToUpper(m[1]) + ":/" + p[len(m[0]):]
		}
		// C:foo is relative to drive C's own folder, unknown unless C is
		// the drive of cwd.
		if driveRe.MatchString(p) && (len(p) > 2 && p[2] != '/') {
			if !strings.EqualFold(p[:2], firstN(slash(cwd), 2)) {
				return p, ZoneUnknown
			}
			p = slash(cwd) + "/" + p[2:]
		}
	} else {
		if strings.HasPrefix(p, "//") && dialect != Bash {
			return e.clean(p), ZoneUNC
		}
		switch {
		case p == "/dev/null" || p == "/dev/stdout" || p == "/dev/stderr" || p == "/dev/stdin" || p == "/dev/tty" ||
			p == "/dev/zero" || p == "/dev/urandom" || p == "/dev/random" || strings.HasPrefix(p, "/dev/fd/"):
			return p, ZoneNull
		case strings.HasPrefix(p, "/dev/"):
			return p, ZoneDevice
		}
	}
	if strings.HasPrefix(strings.ToLower(p), "smb://") {
		return p, ZoneUNC
	}
	if cwd == "" && !e.isAbs(p) {
		return p, ""
	}
	return e.join(slash(cwd), p), ""
}

func firstN(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}

// msys maps the paths Git Bash on Windows understands: /c/x is C:\x, /tmp
// is the user's temp folder, /dev/null the null device, and /usr, /etc,
// /bin and / itself are inside Git's install, which counts as system.
func (e Env) msys(p string) (Zone, string, bool) {
	switch {
	case p == "/dev/null" || p == "/dev/stdout" || p == "/dev/stderr" || p == "/dev/stdin" || p == "/dev/tty" ||
		p == "/dev/zero" || p == "/dev/urandom" || p == "/dev/random" || strings.HasPrefix(p, "/dev/fd/"):
		return ZoneNull, "", true
	case strings.HasPrefix(p, "/dev/"):
		return ZoneDevice, "", true
	case strings.HasPrefix(p, "//"):
		return "", p, true
	}
	if m := msysDriveRe.FindStringSubmatch(p); m != nil {
		return "", strings.ToUpper(m[1]) + ":/" + p[len(m[0]):], true
	}
	if p == "/tmp" || strings.HasPrefix(p, "/tmp/") {
		if t, ok := e.variable("TEMP"); ok {
			return "", t + strings.TrimPrefix(p, "/tmp"), true
		}
		return ZoneTemp, "", true
	}
	if strings.HasPrefix(p, "/mnt/") {
		return "", "", false
	}
	if strings.HasPrefix(p, "/") {
		return ZoneSystem, "", true
	}
	return "", "", false
}

// systemDirs are the operating system's folders on Linux and macOS.
var systemDirs = []string{"/etc", "/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/opt", "/boot",
	"/proc", "/sys", "/var", "/System", "/Library", "/Applications", "/private/etc", "/private/var", "/root", "/srv",
	"/snap"}

// zoneOf decides the zone of an absolute path.
func (e Env) zoneOf(p string) Zone {
	if p == "" || !e.isAbs(p) {
		return ZoneUnknown
	}
	ws := slash(e.Workspace)
	switch {
	case e.ritam(p):
		return ZoneRitam
	case e.protected(p):
		return ZoneProtected
	case e.Build != "" && e.within(e.Build, p):
		return ZoneBuild
	case ws != "" && e.within(ws, p):
		return ZoneWorkspace
	}
	for _, t := range e.Temp {
		if e.within(t, p) {
			return ZoneTemp
		}
	}
	if e.OS != "windows" && (p == "/tmp" || strings.HasPrefix(p, "/tmp/") || strings.HasPrefix(p, "/var/tmp/") ||
		strings.HasPrefix(p, "/private/tmp/") || strings.HasPrefix(p, "/private/var/folders/")) {
		return ZoneTemp
	}
	if e.Home != "" && e.within(e.Home, p) {
		return ZoneHome
	}
	if e.system(p) {
		return ZoneSystem
	}
	return ZoneOutside
}

func (e Env) system(p string) bool {
	if e.OS == "windows" {
		if len(p) <= 3 && driveRe.MatchString(p) {
			return true // a drive's root
		}
		for _, v := range []string{"WINDIR", "PROGRAMDATA"} {
			if d, ok := e.variable(v); ok && e.within(d, p) {
				return true
			}
		}
		lower := strings.ToLower(p)
		return len(lower) > 3 && (strings.HasPrefix(lower[3:], "windows") && (len(lower) == 10 || lower[10] == '/') ||
			strings.HasPrefix(lower[3:], "program files") || strings.HasPrefix(lower[3:], "programdata"))
	}
	if p == "/" {
		return true
	}
	for _, d := range systemDirs {
		if e.within(d, p) {
			return true
		}
	}
	return false
}

func (e Env) ritam(p string) bool {
	if ws := slash(e.Workspace); ws != "" {
		if e.within(ws+"/.ritam", p) || e.samePath(ws+"/ritam.yaml", p) {
			return true
		}
	}
	return e.RitamHome != "" && e.within(e.RitamHome, p)
}

func (e Env) protected(p string) bool {
	list := e.Protected
	if list == nil {
		list = DefaultProtected
	}
	ws := slash(e.Workspace)
	for _, pat := range list {
		pat = slash(pat)
		if !e.isAbs(pat) {
			if ws == "" {
				continue
			}
			pat = ws + "/" + pat
		}
		if strings.ContainsAny(pat, "*?") {
			if e.globMatch(pat, p) {
				return true
			}
		} else if e.within(pat, p) {
			return true
		}
	}
	return false
}

func (e Env) samePath(a, b string) bool {
	a, b = e.clean(slash(a)), e.clean(slash(b))
	if e.caseless() {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// zoneRank orders zones from harmless to dangerous, for picking the worse
// of two.
var zoneRank = map[Zone]int{ZoneNull: 0, ZoneBuild: 1, ZoneWorkspace: 2, ZoneTemp: 3, ZoneProtected: 4, ZoneRitam: 5,
	ZoneHome: 6, ZoneOutside: 7, ZoneUnknown: 8, ZoneUNC: 9, ZoneSystem: 10, ZoneDevice: 11, ZoneRegistry: 12}

func worse(a, b Zone) Zone {
	if zoneRank[b] > zoneRank[a] {
		return b
	}
	return a
}

// MatchGlob reports whether the path p (as Path.Path holds it) matches the
// rules-file glob pattern: "**" any number of folders, "*" anything within
// one name, "?" one character; "dir/**" matches dir itself too. Pattern
// placeholders: "~" the home folder, "{workspace}", "{build}", and
// %APPDATA%-style variables. A pattern starting with "**/" matches at any
// depth. A placeholder with no value never matches.
func (e Env) MatchGlob(pattern, p string) bool {
	pat, ok := e.expandPattern(pattern)
	if !ok {
		return false
	}
	return e.globMatch(pat, p)
}

var pctVarRe = regexp.MustCompile(`%([A-Za-z_]+)%`)

func (e Env) expandPattern(pattern string) (string, bool) {
	pat := slash(pattern)
	ok := true
	switch {
	case pat == "~" || strings.HasPrefix(pat, "~/"):
		if e.Home == "" {
			return "", false
		}
		pat = slash(e.Home) + pat[1:]
	case strings.HasPrefix(pat, "{workspace}"):
		if e.Workspace == "" {
			return "", false
		}
		pat = slash(e.Workspace) + strings.TrimPrefix(pat, "{workspace}")
	case strings.HasPrefix(pat, "{build}"):
		if e.Build == "" {
			return "", false
		}
		pat = slash(e.Build) + strings.TrimPrefix(pat, "{build}")
	}
	pat = pctVarRe.ReplaceAllStringFunc(pat, func(m string) string {
		v, found := e.variable(strings.Trim(m, "%"))
		if !found {
			ok = false
		}
		return v
	})
	return pat, ok
}

// globMatch matches an expanded pattern against a path.
func (e Env) globMatch(pat, p string) bool {
	p = slash(p)
	if e.caseless() || strings.HasPrefix(p, "HK") {
		pat, p = strings.ToLower(pat), strings.ToLower(p)
	}
	ps := strings.Split(strings.Trim(pat, "/"), "/")
	xs := strings.Split(strings.Trim(p, "/"), "/")
	if strings.HasPrefix(pat, "**/") {
		ps = strings.Split(strings.TrimPrefix(pat, "**/"), "/")
		for i := range xs {
			if matchSegs(ps, xs[i:]) {
				return true
			}
		}
		return false
	}
	return matchSegs(ps, xs)
}

func matchSegs(ps, xs []string) bool {
	for len(ps) > 0 {
		if ps[0] == "**" {
			rest := ps[1:]
			for i := 0; i <= len(xs); i++ {
				if matchSegs(rest, xs[i:]) {
					return true
				}
			}
			return false
		}
		if len(xs) == 0 {
			return false
		}
		if ok, _ := path.Match(ps[0], xs[0]); !ok {
			return false
		}
		ps, xs = ps[1:], xs[1:]
	}
	return len(xs) == 0
}
