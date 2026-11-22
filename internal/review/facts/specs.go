package facts

// The data tables of the parser: which flags of a program take a value,
// which arguments are paths and what is done to them, which programs wrap
// another command, which flags carry inline code, and PowerShell's aliases
// and parameters. They cover the programs the rules name; a program missing
// here is still parsed, only more roughly (every flag is a bare flag, every
// path-looking argument is read).

// spec is what the parser knows about one program's arguments.
type spec struct {
	// values are the flags taking a value as the next word (or glued to a
	// short flag: -ofile). Long flags may also take it after "=".
	values []string
	// global are options before the subcommand that take a value (git -C
	// dir push): they are skipped when working out the subcommand.
	global []string
}

var specs = map[string]spec{
	"git": {global: []string{"-C", "-c", "--git-dir", "--work-tree", "--namespace", "--exec-path", "--config-env"},
		values: []string{"-m", "-F", "-b", "-B", "-o", "-u", "--author", "--message", "--file", "--branch", "--depth",
			"--origin", "--upload-pack", "--receive-pack", "--exec", "-X", "-s", "--strategy", "--template", "-x",
			"--exclude", "-e"}},
	"docker":    {global: []string{"-H", "--host", "--context", "--config", "-l", "--log-level"}, values: []string{"-v", "--volume", "--mount", "-e", "--env", "-p", "--publish", "--name", "-w", "--workdir", "-u", "--user", "--network", "--entrypoint", "-f", "--file", "-t", "--tag", "--env-file", "--cap-add", "--security-opt", "--pid", "--ipc", "--device"}},
	"podman":    {global: []string{"-H", "--host", "--context", "--config", "-l"}, values: []string{"-v", "--volume", "--mount", "-e", "--env", "-p", "--name", "-w", "-u", "--entrypoint"}},
	"kubectl":   {global: []string{"-n", "--namespace", "--context", "--kubeconfig", "--cluster", "--user", "-s", "--server"}, values: []string{"-f", "--filename", "-c", "--container", "-o", "--output", "-l", "--selector"}},
	"oc":        {global: []string{"-n", "--namespace", "--context", "--kubeconfig", "--cluster", "--user"}, values: []string{"-f", "-c", "-o"}},
	"aws":       {global: []string{"--profile", "--region", "--output", "--endpoint-url"}},
	"gcloud":    {global: []string{"--project", "--account", "--format", "--configuration"}},
	"az":        {global: []string{"--subscription", "-o", "--output"}},
	"npm":       {global: []string{"--prefix", "-w", "--workspace", "-C", "--dir", "--filter"}, values: []string{"--registry", "--tag", "--otp"}},
	"pnpm":      {global: []string{"--prefix", "-w", "--workspace", "-C", "--dir", "--filter"}, values: []string{"--registry", "--tag"}},
	"yarn":      {global: []string{"--prefix", "-w", "--workspace", "-C", "--cwd", "--dir", "--filter"}, values: []string{"--registry"}},
	"cargo":     {global: []string{"-Z", "--manifest-path", "--config"}, values: []string{"--index", "--registry", "--git", "--path", "--target", "-p", "--package", "--token"}},
	"go":        {global: []string{"-C"}, values: []string{"-o", "-ldflags", "-gcflags", "-tags", "-run", "-bench", "-count", "-timeout", "-exec", "-toolexec", "-modfile", "-p"}},
	"terraform": {global: []string{"-chdir"}, values: []string{"-var", "-var-file", "-target", "-out"}},
	"tofu":      {global: []string{"-chdir"}, values: []string{"-var", "-var-file", "-target", "-out"}},
	"helm":      {global: []string{"-n", "--namespace", "--kube-context"}, values: []string{"-f", "--values", "--set", "--version"}},
	"pip":       {values: []string{"-r", "-c", "-e", "-i", "--index-url", "--extra-index-url", "-f", "--find-links", "-t", "--target", "--prefix", "--trusted-host", "--root"}},
	"pip3":      {values: []string{"-r", "-c", "-e", "-i", "--index-url", "--extra-index-url", "-f", "--find-links", "-t", "--target", "--prefix", "--trusted-host", "--root"}},
	"uv":        {values: []string{"--index-url", "--extra-index-url", "--index", "--default-index", "-p", "--python", "-r", "--with", "--from"}},
	"curl": {values: []string{"-o", "--output", "-d", "--data", "--data-raw", "--data-binary", "--data-urlencode", "--json",
		"-F", "--form", "-H", "--header", "-T", "--upload-file", "-X", "--request", "-u", "--user", "-A", "--user-agent",
		"-e", "--referer", "-x", "--proxy", "-b", "--cookie", "-c", "--cookie-jar", "-K", "--config", "-w", "--write-out",
		"-m", "--max-time", "--url", "--connect-timeout", "--retry", "-r", "--range", "--cacert", "--cert", "--key",
		"--resolve", "--connect-to", "-E", "--interface", "--output-dir"}},
	"wget":      {values: []string{"-O", "--output-document", "-P", "--directory-prefix", "-o", "--output-file", "-a", "-i", "--input-file", "-U", "--user-agent", "--header", "-e", "--execute", "--post-data", "--post-file", "--body-file", "-t", "--tries", "-T", "--timeout", "--user", "--password"}},
	"ssh":       {values: []string{"-o", "-i", "-p", "-l", "-L", "-R", "-D", "-F", "-J", "-W", "-b", "-c", "-E", "-S", "-m", "-O", "-Q", "-w", "-e", "-B", "-I"}},
	"scp":       {values: []string{"-o", "-i", "-P", "-l", "-F", "-J", "-c", "-S", "-D", "-X"}},
	"sftp":      {values: []string{"-o", "-i", "-P", "-l", "-F", "-J", "-c", "-S", "-b", "-D", "-R", "-s"}},
	"tar":       {values: []string{"-f", "--file", "-C", "--directory", "-T", "--files-from", "-X", "--exclude-from", "--to-command", "--checkpoint-action", "-I", "--use-compress-program", "--exclude", "-H", "--format", "-b", "-g"}},
	"rsync":     {values: []string{"-e", "--rsh", "--rsync-path", "--exclude", "--include", "--filter", "-f", "--exclude-from", "--include-from", "--files-from", "--backup-dir", "--temp-dir", "-T", "--partial-dir", "--log-file", "--password-file"}},
	"sudo":      {values: []string{"-u", "-g", "-h", "-p", "-C", "-D", "-r", "-t", "-U", "--user", "--group", "--host", "--prompt", "--chdir"}},
	"xargs":     {values: []string{"-I", "-i", "-n", "-P", "-L", "-l", "-d", "-a", "-E", "-e", "-s", "--max-args", "--max-procs", "--delimiter", "--arg-file", "--replace", "--max-lines"}},
	"find":      {values: []string{"-name", "-iname", "-path", "-ipath", "-regex", "-iregex", "-type", "-newer", "-user", "-group", "-perm", "-size", "-mtime", "-mmin", "-atime", "-ctime", "-maxdepth", "-mindepth", "-wholename", "-fstype", "-printf", "-fprint", "-fprintf", "-fls", "-samefile", "-links", "-inum", "-uid", "-gid", "-used", "-cmin", "-amin"}},
	"grep":      {values: []string{"-e", "--regexp", "-f", "--file", "-m", "--max-count", "-A", "-B", "-C", "--include", "--exclude", "--exclude-dir", "--color", "--colour", "-d", "-D", "--label"}},
	"rg":        {values: []string{"-e", "--regexp", "-f", "--file", "-m", "--max-count", "-A", "-B", "-C", "-g", "--glob", "-t", "--type", "-T", "--type-not", "--pre", "-j", "--threads", "-M", "--max-columns", "--max-depth", "-r", "--replace", "--sort", "--sortr", "--color", "--colors", "--engine"}},
	"sed":       {values: []string{"-e", "--expression", "-f", "--file", "-l", "--line-length"}},
	"awk":       {values: []string{"-f", "-v", "-F", "--file", "--assign", "--field-separator"}},
	"gawk":      {values: []string{"-f", "-v", "-F", "-e", "--source"}},
	"truncate":  {values: []string{"-s", "--size", "-r", "--reference"}},
	"chmod":     {values: []string{"--reference"}},
	"chown":     {values: []string{"--reference", "--from"}},
	"ln":        {values: []string{"-t", "--target-directory", "-S", "--suffix"}},
	"cp":        {values: []string{"-t", "--target-directory", "-S", "--suffix", "--reflink", "--sparse", "--backup"}},
	"mv":        {values: []string{"-t", "--target-directory", "-S", "--suffix", "--backup"}},
	"install":   {values: []string{"-t", "--target-directory", "-m", "--mode", "-o", "--owner", "-g", "--group", "-S", "--suffix"}},
	"unzip":     {values: []string{"-d", "-x", "-P"}},
	"zip":       {values: []string{"-x", "-i", "-P", "--password", "-b", "-n", "-t", "-tt"}},
	"nc":        {values: []string{"-e", "-c", "-p", "-s", "-w", "-x", "-X", "-i", "-q", "-o", "-I", "-O", "-T", "-V", "-P"}},
	"ncat":      {values: []string{"-e", "--exec", "-c", "--sh-exec", "--lua-exec", "-p", "-s", "-w", "-x", "--proxy", "-i", "-o", "--output", "-m", "--max-conns", "--allow", "--deny"}},
	"socat":     {values: []string{"-d", "-lf", "-ly", "-b", "-T", "-t"}},
	"timeout":   {values: []string{"-s", "--signal", "-k", "--kill-after"}},
	"nice":      {values: []string{"-n", "--adjustment"}},
	"ionice":    {values: []string{"-c", "-n", "-p", "--class", "--classdata"}},
	"stdbuf":    {values: []string{"-i", "-o", "-e", "--input", "--output", "--error"}},
	"chrt":      {values: []string{"-p"}},
	"taskset":   {values: []string{"-p", "-c", "--cpu-list"}},
	"env":       {values: []string{"-u", "--unset", "-C", "--chdir", "-S", "--split-string"}},
	"doas":      {values: []string{"-u", "-C"}},
	"su":        {values: []string{"-c", "--command", "-s", "--shell", "-g", "-G", "-w"}},
	"crontab":   {values: []string{"-u"}},
	"systemctl": {values: []string{"-H", "--host", "-M", "--machine", "-t", "--type", "-p", "--property", "--state", "-o", "--output", "-n", "--lines", "--root"}},
	"python":    {values: []string{"-c", "-m", "-W", "-X", "--check-hash-based-pycs"}},
	"python3":   {values: []string{"-c", "-m", "-W", "-X"}},
	"py":        {values: []string{"-c", "-m", "-W", "-X"}},
	"node":      {values: []string{"-e", "--eval", "-p", "--print", "-r", "--require", "--import", "--loader", "--experimental-loader", "--inspect-port", "--title", "--input-type", "-C", "--conditions"}},
	"perl":      {values: []string{"-e", "-E", "-I", "-M", "-m", "-x"}},
	"ruby":      {values: []string{"-e", "-I", "-r", "-C", "-E", "-F"}},
	"php":       {values: []string{"-r", "-d", "-c", "-f", "-B", "-R", "-F", "-E", "-z", "-t", "-S"}},
	"deno":      {values: []string{"--allow-read", "--allow-write", "--allow-net", "--allow-env", "--allow-run", "-c", "--config", "--import-map", "--location", "--cert", "--v8-flags", "--seed"}},
	"bun":       {values: []string{"-e", "--eval", "-p", "--print", "-r", "--preload", "--cwd", "--env-file", "--config", "-c"}},
	"lua":       {values: []string{"-e", "-l"}},
	"osascript": {values: []string{"-e", "-l", "-s"}},
	"openssl":   {values: []string{"-in", "-out", "-inkey", "-key", "-passin", "-passout", "-pass", "-k", "-kfile", "-connect", "-servername", "-cert", "-CAfile", "-subj", "-days", "-newkey", "-config", "-md", "-iv", "-K", "-S"}},
	"base64":    {values: []string{"-w", "--wrap", "-i", "-o", "--input", "--output", "-b", "--break"}},
	"dd":        {},
	"npx":       {values: []string{"-p", "--package", "-c", "--call"}},
	"pnpx":      {values: []string{"-p", "--package", "-c"}},
	"bunx":      {values: []string{"-p", "--package"}},
	"uvx":       {values: []string{"--from", "--with", "-p", "--python", "--index-url", "--index"}},
	"pipx":      {values: []string{"--spec", "--python", "--index-url", "--pip-args"}},
	"conda":     {values: []string{"-n", "--name", "-p", "--prefix", "-c", "--channel"}},
	"watch":     {values: []string{"-n", "--interval", "-d", "--differences", "-t"}},
	"flock":     {values: []string{"-w", "--timeout", "-E", "--conflict-exit-code", "-c", "--command"}},
	"script":    {values: []string{"-c", "--command", "-T", "--log-timing", "-I", "-O", "-B", "-E", "--echo"}},
	"parallel":  {values: []string{"-j", "--jobs", "-S", "--sshlogin", "--sshloginfile", "-a", "--arg-file", "--colsep", "-d", "--delimiter", "-I", "-E"}},
	"wsl":       {values: []string{"-d", "--distribution", "-u", "--user", "--cd", "--shell-type"}},
}

// wrapperKind says how a wrapper's inner command is found.
type wrapperKind int

const (
	// wrapOpts: skip the wrapper's options (and the values of those its
	// spec lists); the rest is the inner command.
	wrapOpts wrapperKind = iota + 1
	wrapEnv
	wrapTimeout
	wrapCommand
	wrapXargs
	wrapFind
	// wrapInline: the value of an option (-c) is a bash script.
	wrapInline
	wrapParallel
	wrapBusybox
	wrapPythonM
	// wrapPackage: the package's command (npx, uvx) runs; the wrapper's
	// record stays for the supply-chain rules.
	wrapPackage
	// wrapSub: a subcommand runs the rest (uv run, poetry run, npm exec).
	wrapSub
	wrapCmdC
	wrapStart
	wrapPowerShell
	wrapWSL
	wrapRemote
	// wrapCmdString: an option's value is a cmd command line (schtasks /tr).
	wrapCmdString
	wrapStartProcess
	wrapInvokeExpression
	wrapScriptBlock
)

var wrappers = map[string]wrapperKind{
	"sudo": wrapOpts, "doas": wrapOpts, "pkexec": wrapOpts, "gsudo": wrapOpts, "run0": wrapOpts,
	"nohup": wrapOpts, "time": wrapOpts, "setsid": wrapOpts, "unbuffer": wrapOpts, "exec": wrapOpts,
	"builtin": wrapOpts, "nice": wrapOpts, "ionice": wrapOpts, "stdbuf": wrapOpts, "chrt": wrapOpts,
	"taskset": wrapOpts, "caffeinate": wrapOpts, "firejail": wrapOpts, "torsocks": wrapOpts,
	"proxychains": wrapOpts, "proxychains4": wrapOpts, "chroot": wrapOpts, "runuser": wrapOpts,
	"env": wrapEnv, "timeout": wrapTimeout, "gtimeout": wrapTimeout, "command": wrapCommand,
	"xargs": wrapXargs, "find": wrapFind,
	"su": wrapInline, "script": wrapInline, "watch": wrapInline, "flock": wrapInline,
	"parallel": wrapParallel, "busybox": wrapBusybox,
	"python": wrapPythonM, "python3": wrapPythonM, "py": wrapPythonM, "python2": wrapPythonM,
	"npx": wrapPackage, "bunx": wrapPackage, "pnpx": wrapPackage, "uvx": wrapPackage,
	"npm": wrapSub, "pnpm": wrapSub, "yarn": wrapSub, "pipx": wrapSub, "uv": wrapSub, "poetry": wrapSub,
	"pipenv": wrapSub, "pdm": wrapSub, "conda": wrapSub, "bundle": wrapSub, "dotnet": wrapSub, "hatch": wrapSub,
	"rye": wrapSub, "mamba": wrapSub, "micromamba": wrapSub, "pixi": wrapSub, "deno": wrapSub, "bun": wrapSub,
	"cmd": wrapCmdC, "start": wrapStart,
	"powershell": wrapPowerShell, "pwsh": wrapPowerShell, "powershell_ise": wrapPowerShell,
	"wsl": wrapWSL, "ssh": wrapRemote, "docker": wrapRemote, "podman": wrapRemote, "kubectl": wrapRemote,
	"oc":       wrapRemote,
	"forfiles": wrapCmdString, "schtasks": wrapCmdString, "wmic": wrapCmdString, "conhost": wrapOpts,
	"runas":         wrapCmdString,
	"start-process": wrapStartProcess, "invoke-expression": wrapInvokeExpression,
	"system.management.automation.scriptblock::create": wrapInvokeExpression,
	"invoke-command": wrapScriptBlock, "start-job": wrapScriptBlock, "start-threadjob": wrapScriptBlock,
	"foreach-object": wrapScriptBlock, "where-object": wrapScriptBlock,
}

// subRunners are the subcommands of wrapSub programs that run the rest of
// the line as a command, with the options they take a value for.
var subRunners = map[string]struct {
	sub    []string
	values []string
}{
	"npm":        {sub: []string{"exec", "x"}, values: []string{"-p", "--package", "-c", "--call", "-w", "--workspace"}},
	"pnpm":       {sub: []string{"dlx", "exec"}, values: []string{"-p", "--package", "-C", "--dir", "--filter"}},
	"yarn":       {sub: []string{"dlx", "exec"}, values: []string{"-p", "--package"}},
	"pipx":       {sub: []string{"run"}, values: []string{"--spec", "--python", "--index-url", "--pip-args"}},
	"uv":         {sub: []string{"run", "tool run"}, values: []string{"--with", "--from", "-p", "--python", "--index-url", "--index", "--directory", "--project", "--group", "--extra", "--package"}},
	"poetry":     {sub: []string{"run"}},
	"pipenv":     {sub: []string{"run"}},
	"pdm":        {sub: []string{"run"}, values: []string{"-p", "--project"}},
	"hatch":      {sub: []string{"run"}},
	"rye":        {sub: []string{"run"}},
	"conda":      {sub: []string{"run"}, values: []string{"-n", "--name", "-p", "--prefix", "--cwd"}},
	"mamba":      {sub: []string{"run"}, values: []string{"-n", "--name", "-p", "--prefix"}},
	"micromamba": {sub: []string{"run"}, values: []string{"-n", "--name", "-p", "--prefix"}},
	"pixi":       {sub: []string{"run", "exec"}, values: []string{"-e", "--environment", "--manifest-path"}},
	"bundle":     {sub: []string{"exec"}},
	"dotnet":     {sub: []string{"tool run"}},
}

// packageRunners: a package-running subcommand counts as running the
// package's command (npm exec x), like npx.
var packageRunners = map[string]bool{"npm exec": true, "npm x": true, "pnpm dlx": true, "yarn dlx": true,
	"pipx run": true, "uv tool run": true}

// inlineFlags are the flags whose value is code, per interpreter, and the
// language of that code.
var inlineFlags = map[string]struct {
	flags []string
	lang  string
}{
	"bash": {[]string{"-c"}, Bash}, "sh": {[]string{"-c"}, Bash}, "zsh": {[]string{"-c"}, Bash},
	"dash": {[]string{"-c"}, Bash}, "ksh": {[]string{"-c"}, Bash}, "mksh": {[]string{"-c"}, Bash},
	"ash": {[]string{"-c"}, Bash}, "fish": {[]string{"-c", "--command"}, "fish"}, "csh": {[]string{"-c"}, "csh"},
	"tcsh":   {[]string{"-c"}, "csh"},
	"python": {[]string{"-c"}, "python"}, "python3": {[]string{"-c"}, "python"}, "python2": {[]string{"-c"}, "python"},
	"py": {[]string{"-c"}, "python"}, "pypy": {[]string{"-c"}, "python"}, "pypy3": {[]string{"-c"}, "python"},
	"node": {[]string{"-e", "--eval", "-p", "--print"}, "javascript"}, "nodejs": {[]string{"-e", "--eval", "-p", "--print"}, "javascript"},
	"perl": {[]string{"-e", "-E"}, "perl"}, "ruby": {[]string{"-e"}, "ruby"}, "php": {[]string{"-r"}, "php"},
	"bun": {[]string{"-e", "--eval", "-p", "--print"}, "javascript"}, "osascript": {[]string{"-e"}, "applescript"},
	"lua": {[]string{"-e"}, "lua"}, "luajit": {[]string{"-e"}, "lua"},
	"powershell": {[]string{"-command", "-c", "-encodedcommand", "-ec", "-enc", "-e"}, PowerShell},
	"pwsh":       {[]string{"-command", "-c", "-encodedcommand", "-ec", "-enc", "-e"}, PowerShell},
	"cmd":        {[]string{"/c", "/k", "/r"}, Cmd},
}

// shells are the programs that run bash-like code.
var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "mksh": true,
	"ash": true, "busybox": true}

// interpreters run a script file named as their first argument.
var interpreters = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true,
	"python": true, "python2": true, "python3": true, "py": true, "pypy": true, "pypy3": true, "perl": true,
	"ruby": true, "node": true, "nodejs": true, "deno": true, "bun": true, "php": true, "lua": true, "luajit": true,
	"tclsh": true, "osascript": true, "groovy": true, "julia": true, "rscript": true, "wscript": true,
	"cscript": true, "mshta": true, "source": true, ".": true}

// awks take their program as the first argument.
var awks = map[string]bool{"awk": true, "gawk": true, "mawk": true, "nawk": true}

// winSwitches are the programs whose flags are written /x.
var winSwitches = map[string]bool{"reg": true, "schtasks": true, "icacls": true, "takeown": true, "attrib": true,
	"net": true, "netsh": true, "sc": true, "wmic": true, "bcdedit": true, "vssadmin": true, "wbadmin": true,
	"cipher": true, "format": true, "diskpart": true, "robocopy": true, "xcopy": true, "forfiles": true,
	"runas": true, "certutil": true, "bitsadmin": true, "cmd": true, "start": true, "shutdown": true,
	"taskkill": true, "fsutil": true, "mklink": true, "del": true, "erase": true, "rd": true, "rmdir": true,
	"copy": true, "move": true, "dir": true, "subst": true, "setx": true, "wevtutil": true, "auditpol": true,
	"msiexec": true, "dism": true, "sfc": true, "chkdsk": true, "manage-bde": true, "cacls": true,
	"compact": true, "where": true, "findstr": true, "type": true, "ren": true, "rename": true, "md": true,
	"mkdir": true, "assoc": true, "ftype": true, "tasklist": true, "systeminfo": true, "whoami": true,
	"regsvr32": true, "rundll32": true, "mshta": true, "cscript": true, "wscript": true, "pnputil": true,
	"powercfg": true, "w32tm": true, "gpupdate": true, "secedit": true, "nltest": true, "dsquery": true,
	"klist": true, "cmdkey": true, "vaultcmd": true, "ntdsutil": true, "esentutl": true, "expand": true,
	"extrac32": true, "makecab": true, "replace": true, "tree": true, "timeout": true}

// access describes the paths among a program's arguments.
type access struct {
	// all is the access of every non-flag argument ("" = none is a path).
	all Access
	// skip is how many non-flag arguments come first and are not paths
	// (grep's pattern, chmod's mode).
	skip int
	// last, when set, is the access of the last argument instead (cp's
	// destination), if there are at least two.
	last Access
	// second, when set, is the access of the second argument (mklink's
	// target, robocopy's destination).
	first, second Access
	// flags are flags whose value is a path, with its access.
	flags map[string]Access
}

var accessByProgram = map[string]access{
	"rm": {all: Delete}, "unlink": {all: Delete}, "shred": {all: Delete}, "srm": {all: Delete},
	"del": {all: Delete}, "erase": {all: Delete}, "rd": {all: Delete}, "rmdir": {all: Delete},
	"remove-item":   {all: Delete, flags: map[string]Access{"-path": Delete, "-literalpath": Delete}},
	"clear-content": {all: Write, flags: map[string]Access{"-path": Write, "-literalpath": Write}},
	"cp":            {all: Read, last: Write, flags: map[string]Access{"-t": Write, "--target-directory": Write}},
	"copy":          {all: Read, last: Write}, "xcopy": {all: Read, last: Write}, "install": {all: Read, last: Write, flags: map[string]Access{"-t": Write}},
	"copy-item": {all: Read, last: Write, flags: map[string]Access{"-path": Read, "-literalpath": Read, "-destination": Write}},
	"robocopy":  {first: Read, second: Write},
	"mv":        {all: Delete, last: Write, flags: map[string]Access{"-t": Write, "--target-directory": Write}},
	"move":      {all: Delete, last: Write}, "ren": {all: Delete, last: Write}, "rename": {all: Delete, last: Write},
	"move-item":   {all: Delete, last: Write, flags: map[string]Access{"-path": Delete, "-literalpath": Delete, "-destination": Write}},
	"rename-item": {all: Delete, last: Write, flags: map[string]Access{"-path": Delete, "-literalpath": Delete, "-newname": Write}},
	"ln":          {all: Link, last: Write, flags: map[string]Access{"-t": Write, "--target-directory": Write}},
	"mklink":      {first: Write, second: Link},
	"touch":       {all: Write}, "mkdir": {all: Write}, "md": {all: Write}, "tee": {all: Write}, "truncate": {all: Write},
	"out-file":           {all: Write, flags: map[string]Access{"-filepath": Write, "-literalpath": Write}},
	"set-content":        {all: Write, flags: map[string]Access{"-path": Write, "-literalpath": Write}},
	"add-content":        {all: Write, flags: map[string]Access{"-path": Write, "-literalpath": Write}},
	"new-item":           {all: Write, flags: map[string]Access{"-path": Write, "-name": Write, "-target": Link, "-value": Link}},
	"curl":               {flags: map[string]Access{"-o": Write, "--output": Write, "-T": Read, "--upload-file": Read, "-K": Read, "--config": Read, "-c": Write, "--cookie-jar": Write, "--output-dir": Write}},
	"wget":               {flags: map[string]Access{"-O": Write, "--output-document": Write, "-P": Write, "--directory-prefix": Write, "-i": Read, "--input-file": Read, "--post-file": Read}},
	"invoke-webrequest":  {flags: map[string]Access{"-outfile": Write, "-infile": Read}},
	"invoke-restmethod":  {flags: map[string]Access{"-outfile": Write, "-infile": Read}},
	"start-bitstransfer": {flags: map[string]Access{"-destination": Write}},
	"unzip":              {all: Read, flags: map[string]Access{"-d": Write}},
	"cat":                {all: Read}, "less": {all: Read}, "more": {all: Read}, "head": {all: Read}, "tail": {all: Read},
	"type": {all: Read}, "get-content": {all: Read, flags: map[string]Access{"-path": Read, "-literalpath": Read}},
	"sqlite3": {first: Read}, "strings": {all: Read}, "xxd": {all: Read}, "od": {all: Read}, "hexdump": {all: Read},
	"wc": {all: Read}, "sort": {all: Read}, "uniq": {all: Read}, "diff": {all: Read}, "cmp": {all: Read},
	"md5sum": {all: Read}, "sha1sum": {all: Read}, "sha256sum": {all: Read}, "file": {all: Read}, "nl": {all: Read},
	"tac": {all: Read}, "base64": {all: Read}, "base32": {all: Read}, "bat": {all: Read}, "view": {all: Read},
	"grep": {all: Read, skip: 1}, "egrep": {all: Read, skip: 1}, "fgrep": {all: Read, skip: 1}, "rg": {all: Read, skip: 1},
	"ag": {all: Read, skip: 1}, "findstr": {all: Read, skip: 1}, "jq": {all: Read, skip: 1}, "yq": {all: Read, skip: 1},
	"select-string": {flags: map[string]Access{"-path": Read, "-literalpath": Read}},
	"ls":            {all: List}, "dir": {all: List}, "get-childitem": {all: List, flags: map[string]Access{"-path": List, "-literalpath": List}},
	"test-path": {all: List, flags: map[string]Access{"-path": List}}, "stat": {all: List}, "tree": {all: List},
	"du": {all: List}, "realpath": {all: List}, "readlink": {all: List}, "get-item": {all: List, flags: map[string]Access{"-path": List}},
	"cd": {all: List}, "pushd": {all: List}, "chdir": {all: List}, "set-location": {all: List, flags: map[string]Access{"-path": List}},
	"push-location": {all: List},
	"chmod":         {all: Meta, skip: 1}, "chown": {all: Meta, skip: 1}, "chgrp": {all: Meta, skip: 1},
	"icacls": {first: Meta}, "cacls": {first: Meta}, "takeown": {flags: map[string]Access{"/f": Meta}}, "attrib": {all: Meta},
	"xattr": {all: Meta}, "chattr": {all: Meta, skip: 1}, "chflags": {all: Meta, skip: 1},
	"set-acl": {all: Meta, flags: map[string]Access{"-path": Meta}},
	"vim":     {all: Write}, "vi": {all: Write}, "nano": {all: Write}, "emacs": {all: Write}, "notepad": {all: Write},
	"code": {all: Write},
	"sed":  {all: Read, skip: 1}, "awk": {all: Read, skip: 1}, "gawk": {all: Read, skip: 1},
	"zip": {all: Read}, "rsync": {all: Read, last: Write}, "scp": {all: Read, last: Write},
	"dd":     {},
	"source": {first: Exec}, ".": {first: Exec},
	"open": {all: Exec}, "xdg-open": {all: Exec}, "invoke-item": {all: Exec, flags: map[string]Access{"-path": Exec}},
	"git": {}, "tar": {}, "docker": {}, "podman": {}, "mount": {}, "sshfs": {},
	"export-csv":       {flags: map[string]Access{"-path": Write, "-literalpath": Write}},
	"start-process":    {flags: map[string]Access{"-filepath": Exec, "-redirectstandardoutput": Write, "-redirectstandarderror": Write}},
	"expand-archive":   {flags: map[string]Access{"-path": Read, "-destinationpath": Write}},
	"compress-archive": {flags: map[string]Access{"-path": Read, "-destinationpath": Write}},
}

// psAliases are PowerShell's built-in aliases (Windows PowerShell 5.1,
// which Codex starts on Windows: curl, wget and sc are aliases there).
var psAliases = map[string]string{
	"ac": "add-content", "cat": "get-content", "cd": "set-location", "chdir": "set-location",
	"clc": "clear-content", "clear": "clear-host", "cls": "clear-host", "copy": "copy-item", "cp": "copy-item",
	"cpi": "copy-item", "curl": "invoke-webrequest", "del": "remove-item", "dir": "get-childitem",
	"echo": "write-output", "erase": "remove-item", "gc": "get-content", "gci": "get-childitem",
	"gi": "get-item", "gp": "get-itemproperty", "icm": "invoke-command", "iex": "invoke-expression",
	"ii": "invoke-item", "irm": "invoke-restmethod", "iwr": "invoke-webrequest", "ls": "get-childitem",
	"mi": "move-item", "move": "move-item", "mv": "move-item", "ni": "new-item", "popd": "pop-location",
	"pushd": "push-location", "pwd": "get-location", "rd": "remove-item", "ren": "rename-item",
	"ri": "remove-item", "rm": "remove-item", "rmdir": "remove-item", "rni": "rename-item",
	"rp": "remove-itemproperty", "sajb": "start-job", "sal": "set-alias", "saps": "start-process",
	"sc": "set-content", "si": "set-item", "sl": "set-location", "sp": "set-itemproperty",
	"start": "start-process", "type": "get-content", "wget": "invoke-webrequest", "write": "write-output",
	"nal": "new-alias", "gal": "get-alias", "iwmi": "invoke-wmimethod", "sv": "set-variable", "set": "set-variable",
	"%": "foreach-object", "foreach": "foreach-object", "?": "where-object", "where": "where-object",
	"md": "new-item", "mkdir": "new-item", "rv": "remove-variable", "ipmo": "import-module",
	"gcm": "get-command", "kill": "stop-process", "spps": "stop-process", "ps": "get-process", "gps": "get-process",
	"epcsv": "export-csv", "tee": "tee-object", "select": "select-object", "sls": "select-string",
	"gm": "get-member", "fl": "format-list", "ft": "format-table", "measure": "measure-object",
	"sort": "sort-object", "diff": "compare-object", "h": "get-history", "history": "get-history",
	"r": "invoke-history", "ihy": "invoke-history", "clhy": "clear-history", "ise": "powershell_ise",
}

// psParams are the parameters of the cmdlets the rules name, by full
// name, for resolving abbreviations (-rec is -recurse) and aliases, and
// which take a value; positional lists the parameters given by position.
type psCmdlet struct {
	switches   []string
	values     []string
	aliases    map[string]string
	positional []string
}

var psCommon = []string{"-verbose", "-debug", "-erroraction", "-warningaction", "-informationaction",
	"-errorvariable", "-warningvariable", "-informationvariable", "-outvariable", "-outbuffer",
	"-pipelinevariable", "-whatif", "-confirm"}

var psCommonValues = []string{"-erroraction", "-warningaction", "-informationaction", "-errorvariable",
	"-warningvariable", "-informationvariable", "-outvariable", "-outbuffer", "-pipelinevariable"}

var psCmdlets = map[string]psCmdlet{
	"remove-item": {switches: []string{"-recurse", "-force"}, values: []string{"-path", "-literalpath", "-filter", "-include", "-exclude", "-credential", "-stream"},
		aliases: map[string]string{"-r": "-recurse", "-lp": "-literalpath", "-pspath": "-literalpath"}, positional: []string{"-path"}},
	"get-childitem": {switches: []string{"-recurse", "-force", "-name", "-directory", "-file", "-hidden", "-readonly", "-system", "-followsymlink"},
		values:  []string{"-path", "-literalpath", "-filter", "-include", "-exclude", "-depth", "-attributes"},
		aliases: map[string]string{"-s": "-recurse", "-r": "-recurse", "-ad": "-directory", "-af": "-file", "-ah": "-hidden"}, positional: []string{"-path", "-filter"}},
	"copy-item": {switches: []string{"-recurse", "-force", "-container", "-passthru"}, values: []string{"-path", "-literalpath", "-destination", "-filter", "-include", "-exclude", "-credential", "-tosession", "-fromsession"},
		aliases: map[string]string{"-r": "-recurse"}, positional: []string{"-path", "-destination"}},
	"move-item": {switches: []string{"-force", "-passthru"}, values: []string{"-path", "-literalpath", "-destination", "-filter", "-include", "-exclude", "-credential"},
		positional: []string{"-path", "-destination"}},
	"rename-item": {switches: []string{"-force", "-passthru"}, values: []string{"-path", "-literalpath", "-newname", "-credential"},
		positional: []string{"-path", "-newname"}},
	"new-item": {switches: []string{"-force"}, values: []string{"-path", "-name", "-itemtype", "-value", "-target", "-credential"},
		aliases: map[string]string{"-type": "-itemtype"}, positional: []string{"-path"}},
	"set-content": {switches: []string{"-force", "-passthru", "-nonewline", "-asbytestream"}, values: []string{"-path", "-literalpath", "-value", "-encoding", "-filter", "-include", "-exclude", "-stream"},
		positional: []string{"-path", "-value"}},
	"add-content": {switches: []string{"-force", "-passthru", "-nonewline", "-asbytestream"}, values: []string{"-path", "-literalpath", "-value", "-encoding", "-filter", "-include", "-exclude", "-stream"},
		positional: []string{"-path", "-value"}},
	"clear-content": {switches: []string{"-force"}, values: []string{"-path", "-literalpath", "-filter", "-include", "-exclude", "-stream"}, positional: []string{"-path"}},
	"out-file": {switches: []string{"-append", "-force", "-noclobber", "-nonewline"}, values: []string{"-filepath", "-literalpath", "-encoding", "-inputobject", "-width"},
		aliases: map[string]string{"-path": "-filepath"}, positional: []string{"-filepath"}},
	"get-content": {switches: []string{"-raw", "-force", "-wait", "-asbytestream"}, values: []string{"-path", "-literalpath", "-totalcount", "-tail", "-encoding", "-delimiter", "-readcount", "-stream", "-filter", "-include", "-exclude"},
		aliases: map[string]string{"-head": "-totalcount", "-first": "-totalcount", "-last": "-tail"}, positional: []string{"-path"}},
	"invoke-webrequest": {switches: []string{"-usebasicparsing", "-usedefaultcredentials", "-passthru", "-skipcertificatecheck"}, values: []string{"-uri", "-outfile", "-method", "-body", "-headers", "-infile", "-useragent", "-proxy", "-credential", "-contenttype", "-websession", "-sessionvariable", "-timeoutsec", "-maximumredirection"},
		aliases: map[string]string{"-url": "-uri"}, positional: []string{"-uri"}},
	"invoke-restmethod": {switches: []string{"-usebasicparsing", "-usedefaultcredentials", "-passthru", "-skipcertificatecheck"}, values: []string{"-uri", "-outfile", "-method", "-body", "-headers", "-infile", "-useragent", "-proxy", "-credential", "-contenttype", "-websession", "-sessionvariable", "-timeoutsec"},
		aliases: map[string]string{"-url": "-uri"}, positional: []string{"-uri"}},
	"invoke-expression": {values: []string{"-command"}, positional: []string{"-command"}},
	"start-process": {switches: []string{"-wait", "-nonewwindow", "-passthru", "-usenewenvironment", "-loaduserprofile"}, values: []string{"-filepath", "-argumentlist", "-verb", "-workingdirectory", "-windowstyle", "-credential", "-redirectstandardoutput", "-redirectstandarderror", "-redirectstandardinput"},
		aliases: map[string]string{"-args": "-argumentlist", "-path": "-filepath"}, positional: []string{"-filepath", "-argumentlist"}},
	"invoke-command": {switches: []string{"-asjob", "-hidecomputername", "-noprofile"}, values: []string{"-scriptblock", "-computername", "-session", "-argumentlist", "-filepath", "-credential", "-jobname"},
		aliases: map[string]string{"-cn": "-computername", "-args": "-argumentlist"}, positional: []string{"-scriptblock"}},
	"set-location": {switches: []string{"-passthru"}, values: []string{"-path", "-literalpath", "-stackname"}, positional: []string{"-path"}},
	"set-itemproperty": {switches: []string{"-force", "-passthru"}, values: []string{"-path", "-literalpath", "-name", "-value", "-type", "-credential"},
		aliases: map[string]string{"-propertytype": "-type"}, positional: []string{"-path", "-name", "-value"}},
	"new-itemproperty": {switches: []string{"-force"}, values: []string{"-path", "-literalpath", "-name", "-value", "-propertytype", "-credential"},
		aliases: map[string]string{"-type": "-propertytype"}, positional: []string{"-path", "-name", "-value"}},
	"remove-itemproperty": {switches: []string{"-force"}, values: []string{"-path", "-literalpath", "-name", "-credential"}, positional: []string{"-path", "-name"}},
	"set-item":            {switches: []string{"-force", "-passthru"}, values: []string{"-path", "-literalpath", "-value", "-credential"}, positional: []string{"-path", "-value"}},
	"test-path":           {switches: []string{"-isvalid"}, values: []string{"-path", "-literalpath", "-pathtype", "-filter", "-include", "-exclude"}, positional: []string{"-path"}},
	"get-item":            {switches: []string{"-force"}, values: []string{"-path", "-literalpath", "-filter", "-include", "-exclude", "-stream"}, positional: []string{"-path"}},
	"set-acl":             {switches: []string{"-passthru", "-clearcentralaccesspolicy"}, values: []string{"-path", "-literalpath", "-aclobject", "-inputobject"}, positional: []string{"-path", "-aclobject"}},
	"select-string": {switches: []string{"-simplematch", "-casesensitive", "-quiet", "-list", "-notmatch", "-allmatches", "-raw"}, values: []string{"-pattern", "-path", "-literalpath", "-include", "-exclude", "-encoding", "-context"},
		positional: []string{"-pattern", "-path"}},
	"set-alias":               {switches: []string{"-force", "-passthru"}, values: []string{"-name", "-value", "-scope", "-option", "-description"}, positional: []string{"-name", "-value"}},
	"new-alias":               {switches: []string{"-force", "-passthru"}, values: []string{"-name", "-value", "-scope", "-option", "-description"}, positional: []string{"-name", "-value"}},
	"expand-archive":          {switches: []string{"-force", "-passthru"}, values: []string{"-path", "-literalpath", "-destinationpath"}, positional: []string{"-path", "-destinationpath"}},
	"compress-archive":        {switches: []string{"-force", "-update", "-passthru"}, values: []string{"-path", "-literalpath", "-destinationpath", "-compressionlevel"}, positional: []string{"-path", "-destinationpath"}},
	"export-csv":              {switches: []string{"-append", "-force", "-noclobber", "-notypeinformation"}, values: []string{"-path", "-literalpath", "-inputobject", "-delimiter", "-encoding"}, positional: []string{"-path"}},
	"invoke-item":             {values: []string{"-path", "-literalpath", "-filter", "-include", "-exclude"}, positional: []string{"-path"}},
	"start-bitstransfer":      {switches: []string{"-asynchronous"}, values: []string{"-source", "-destination", "-displayname", "-priority", "-transfertype"}, positional: []string{"-source", "-destination"}},
	"register-scheduledtask":  {switches: []string{"-force"}, values: []string{"-taskname", "-action", "-trigger", "-settings", "-principal", "-user", "-password", "-runlevel", "-taskpath", "-xml", "-inputobject", "-description"}, positional: []string{"-taskname"}},
	"new-scheduledtaskaction": {values: []string{"-execute", "-argument", "-workingdirectory", "-id"}, positional: []string{"-execute", "-argument"}},
	"set-executionpolicy":     {switches: []string{"-force"}, values: []string{"-executionpolicy", "-scope"}, positional: []string{"-executionpolicy", "-scope"}},
	"add-mppreference":        {switches: []string{"-force"}, values: []string{"-exclusionpath", "-exclusionprocess", "-exclusionextension", "-exclusionipaddress"}},
	"set-mppreference":        {switches: []string{"-force", "-disablerealtimemonitoring", "-disablebehaviormonitoring", "-disableioavprotection", "-disablescriptscanning", "-disableblockatfirstseen", "-disablearchivescanning", "-disableintrusionpreventionsystem"}, values: []string{"-exclusionpath", "-exclusionprocess", "-exclusionextension", "-mapsreporting", "-submitsamplesconsent", "-enablecontrolledfolderaccess", "-puaprotection"}},
	"stop-process":            {switches: []string{"-force", "-passthru"}, values: []string{"-name", "-id", "-inputobject"}, positional: []string{"-id"}},
	"write-output":            {switches: []string{"-noenumerate"}, values: []string{"-inputobject"}, positional: []string{"-inputobject"}},
	"tee-object":              {switches: []string{"-append"}, values: []string{"-filepath", "-literalpath", "-variable", "-inputobject", "-encoding"}, aliases: map[string]string{"-path": "-filepath"}, positional: []string{"-filepath"}},
	"set-variable":            {switches: []string{"-force", "-passthru"}, values: []string{"-name", "-value", "-scope", "-option", "-visibility", "-description"}, positional: []string{"-name", "-value"}},
	"import-module":           {switches: []string{"-force", "-global", "-passthru"}, values: []string{"-name", "-prefix", "-scope", "-assembly"}, positional: []string{"-name"}},
}
