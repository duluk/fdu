package main

import (
	"os"
	"path/filepath"
	"runtime"
)

// The tables behind -suggest. Every entry names a specific, well-known
// location or a folder name confirmed by a marker file beside it. Nothing is
// suggested just because it is called "cache" or "tmp": a guess that is wrong
// here costs someone their data.

// clearMode says what the generated command removes.
type clearMode int

const (
	clearNone     clearMode = iota // no command; the note says what to do
	clearDir                       // remove the folder itself
	clearContents                  // empty the folder but keep it
)

// action is how a suggestion would be carried out.
type action struct {
	cmd   string // a tool's own cleanup command; takes precedence over clear
	clear clearMode
	sudo  bool
}

// locRule is a fixed location. A path starting with "~/" is repeated for
// every home directory on the machine.
type locRule struct {
	path   string
	title  string
	safety Safety
	note   string
	how    action
}

// nameRule is a folder recognised by name wherever it turns up, as long as
// one of its markers is there too: beside it, or inside it if inside is set.
// No markers means the name alone is unambiguous.
type nameRule struct {
	names   []string
	markers []string
	inside  bool
	title   string
	safety  Safety
	note    string
}

var (
	rmDir      = action{clear: clearDir}
	rmContents = action{clear: clearContents}
)

// Wherever they live, these tools keep their caches in the same place.
var commonLocRules = []locRule{
	{path: "~/.npm/_cacache", title: "npm download cache", safety: Safe,
		how: action{cmd: "npm cache clean --force"}},
	{path: "~/.yarn/berry/cache", title: "Yarn cache", safety: Safe,
		how: action{cmd: "yarn cache clean --all"}},
	{path: "~/go/pkg/mod", title: "Go module cache", safety: Safe,
		note: "re-downloaded on the next build",
		how:  action{cmd: "go clean -modcache"}},
	{path: "~/.cargo/registry/cache", title: "Cargo registry cache", safety: Safe,
		note: "re-downloaded on the next build", how: rmDir},
	{path: "~/.cargo/registry/src", title: "Cargo registry cache", safety: Safe,
		note: "re-downloaded on the next build", how: rmDir},
	{path: "~/.gradle/caches", title: "Gradle caches", safety: Safe,
		note: "re-downloaded on the next build; stop Gradle daemons first", how: rmDir},
	{path: "~/.m2/repository", title: "Maven local repository", safety: Safe,
		note: "re-downloaded on the next build", how: rmDir},
	{path: "~/.nuget/packages", title: "NuGet package cache", safety: Safe,
		how: action{cmd: "dotnet nuget locals all --clear"}},
	{path: "~/.conda/pkgs", title: "Conda package cache", safety: Safe,
		how: action{cmd: "conda clean --all"}},
	{path: "~/miniconda3/pkgs", title: "Conda package cache", safety: Safe,
		how: action{cmd: "conda clean --all"}},
	{path: "~/miniforge3/pkgs", title: "Conda package cache", safety: Safe,
		how: action{cmd: "conda clean --all"}},
	{path: "~/anaconda3/pkgs", title: "Conda package cache", safety: Safe,
		how: action{cmd: "conda clean --all"}},
	{path: "~/.android/avd", title: "Android emulator images", safety: Review,
		note: "delete devices you no longer use from Android Studio's Device Manager"},
	{path: "~/.vagrant.d/boxes", title: "Vagrant boxes", safety: Review,
		how: action{cmd: "vagrant box prune"}},
	{path: "~/.ollama/models", title: "Ollama models", safety: Review,
		note: "remove models you no longer use with `ollama rm <model>`"},
}

// Shared by macOS and Linux.
var unixLocRules = []locRule{
	{path: "~/.cache", title: "User cache folder (.cache)", safety: Safe,
		note: "programs rebuild what they need; quit them first", how: rmContents},
	{path: "~/.cache/huggingface", title: "Hugging Face models and datasets", safety: Review,
		note: "re-downloaded on next use, which can be slow; `huggingface-cli delete-cache` lets you pick"},
}

var darwinLocRules = []locRule{
	{path: "~/Library/Caches", title: "Application caches", safety: Safe,
		note: "apps rebuild these; quit them first", how: rmContents},
	{path: "~/Library/Caches/Homebrew", title: "Homebrew downloads", safety: Safe,
		how: action{cmd: "brew cleanup --prune=all"}},
	{path: "~/Library/Caches/pip", title: "pip cache", safety: Safe,
		how: action{cmd: "pip cache purge"}},
	{path: "~/Library/Caches/go-build", title: "Go build cache", safety: Safe,
		how: action{cmd: "go clean -cache"}},
	{path: "~/Library/Caches/Yarn", title: "Yarn cache", safety: Safe,
		how: action{cmd: "yarn cache clean"}},
	{path: "~/Library/Caches/CocoaPods", title: "CocoaPods cache", safety: Safe,
		how: action{cmd: "pod cache clean --all"}},
	{path: "~/Library/pnpm/store", title: "pnpm store", safety: Safe,
		note: "removes only packages no project references",
		how:  action{cmd: "pnpm store prune"}},
	{path: "~/Library/Logs", title: "Application logs", safety: Safe, how: rmContents},
	{path: "~/.Trash", title: "Trash", safety: Safe,
		note: "empty it from the Finder (File > Empty Trash)"},

	{path: "~/Library/Developer/Xcode/DerivedData", title: "Xcode build products (DerivedData)", safety: Safe,
		note: "rebuilt on the next build", how: rmDir},
	{path: "~/Library/Developer/Xcode/iOS DeviceSupport", title: "Xcode device support files", safety: Safe,
		note: "re-created the next time a device is connected", how: rmContents},
	{path: "~/Library/Developer/Xcode/watchOS DeviceSupport", title: "Xcode device support files", safety: Safe,
		note: "re-created the next time a device is connected", how: rmContents},
	{path: "~/Library/Developer/Xcode/tvOS DeviceSupport", title: "Xcode device support files", safety: Safe,
		note: "re-created the next time a device is connected", how: rmContents},
	{path: "~/Library/Developer/Xcode/visionOS DeviceSupport", title: "Xcode device support files", safety: Safe,
		note: "re-created the next time a device is connected", how: rmContents},
	{path: "~/Library/Developer/CoreSimulator/Caches", title: "Simulator caches", safety: Safe, how: rmDir},
	{path: "~/Library/Developer/Xcode/Archives", title: "Xcode archives", safety: Review,
		note: "needed to symbolicate crash reports from builds you shipped"},
	{path: "~/Library/Developer/CoreSimulator/Devices", title: "Simulator devices", safety: Review,
		note: "removes simulators whose runtime is no longer installed",
		how:  action{cmd: "xcrun simctl delete unavailable"}},
	{path: "/Library/Developer/CoreSimulator/Images", title: "Simulator runtimes", safety: Review,
		note: "remove platforms you no longer use in Xcode > Settings > Components"},

	{path: "~/Library/iTunes/iPhone Software Updates", title: "iPhone and iPad software downloads", safety: Safe,
		note: "re-downloaded if a restore needs them", how: rmContents},
	{path: "~/Library/Application Support/MobileSync/Backup", title: "iPhone and iPad backups", safety: Review,
		note: "manage them in the Finder: select the device, then Manage Backups"},

	{path: "/Library/Caches", title: "System-wide application caches", safety: Safe,
		how: action{clear: clearContents, sudo: true}},
	{path: "/private/tmp", title: "Temporary files (cleared on restart)", safety: Review,
		note: "macOS empties this on restart; files in use by running apps may still be needed"},
	{path: "/private/var/tmp", title: "Long-lived temporary files", safety: Review,
		note: "delete what you recognise; leave anything a running program may be using"},
}

var linuxLocRules = []locRule{
	{path: "~/.cache/pip", title: "pip cache", safety: Safe,
		how: action{cmd: "pip cache purge"}},
	{path: "~/.cache/go-build", title: "Go build cache", safety: Safe,
		how: action{cmd: "go clean -cache"}},
	{path: "~/.cache/yarn", title: "Yarn cache", safety: Safe,
		how: action{cmd: "yarn cache clean"}},
	{path: "~/.local/share/pnpm/store", title: "pnpm store", safety: Safe,
		note: "removes only packages no project references",
		how:  action{cmd: "pnpm store prune"}},
	{path: "~/.local/share/Trash", title: "Trash", safety: Safe, how: rmContents},

	{path: "/var/cache/apt/archives", title: "Downloaded packages (apt)", safety: Safe,
		how: action{cmd: "sudo apt-get clean"}},
	{path: "/var/cache/dnf", title: "Package cache (dnf)", safety: Safe,
		how: action{cmd: "sudo dnf clean all"}},
	{path: "/var/cache/yum", title: "Package cache (yum)", safety: Safe,
		how: action{cmd: "sudo yum clean all"}},
	{path: "/var/cache/pacman/pkg", title: "Package cache (pacman)", safety: Safe,
		how: action{cmd: "sudo pacman -Sc"}},
	{path: "/var/log/journal", title: "System journal", safety: Safe,
		note: "keeps the most recent 200 MB of logs",
		how:  action{cmd: "sudo journalctl --vacuum-size=200M"}},
	{path: "/var/crash", title: "Crash reports", safety: Review,
		how: action{clear: clearContents, sudo: true}},
	{path: "/var/lib/systemd/coredump", title: "Core dumps", safety: Review,
		how: action{clear: clearContents, sudo: true}},
	{path: "/tmp", title: "Temporary files (cleared on restart)", safety: Review,
		note: "usually emptied on restart; files in use by running programs may still be needed"},
	{path: "/var/tmp", title: "Long-lived temporary files", safety: Review,
		note: "delete what you recognise; leave anything a running program may be using"},
}

func windowsLocRules() []locRule {
	sysRoot := windowsRoot()
	drive := filepath.VolumeName(sysRoot) + `\`
	return []locRule{
		{path: "~/AppData/Local/Temp", title: "Temporary files", safety: Safe,
			note: "files still in use will refuse to delete, which is fine", how: rmContents},
		{path: "~/AppData/Local/Microsoft/Windows/INetCache", title: "Internet cache", safety: Safe, how: rmContents},
		{path: "~/AppData/Local/CrashDumps", title: "Crash dumps", safety: Safe, how: rmContents},
		{path: "~/AppData/Local/npm-cache", title: "npm download cache", safety: Safe,
			how: action{cmd: "npm cache clean --force"}},
		{path: "~/AppData/Local/pip/Cache", title: "pip cache", safety: Safe,
			how: action{cmd: "pip cache purge"}},
		{path: "~/AppData/Local/go-build", title: "Go build cache", safety: Safe,
			how: action{cmd: "go clean -cache"}},
		{path: "~/AppData/Local/Yarn/Cache", title: "Yarn cache", safety: Safe,
			how: action{cmd: "yarn cache clean"}},
		{path: "~/AppData/Local/pnpm/store", title: "pnpm store", safety: Safe,
			note: "removes only packages no project references",
			how:  action{cmd: "pnpm store prune"}},
		{path: filepath.Join(sysRoot, "SoftwareDistribution", "Download"), title: "Windows Update downloads", safety: Safe,
			note: "run as administrator", how: rmContents},
		{path: filepath.Join(sysRoot, "Temp"), title: "Windows temporary files", safety: Review,
			note: "run as administrator; files in use will refuse to delete", how: rmContents},
		{path: filepath.Join(drive, "$Recycle.Bin"), title: "Recycle Bin", safety: Safe,
			how: action{cmd: "Clear-RecycleBin"}},
	}
}

// Project build output and dependency folders, found wherever they are.
var commonNameRules = []nameRule{
	{names: []string{"node_modules"}, markers: []string{"package.json"},
		title: "node_modules folders", safety: Safe,
		note: "reinstalled by npm, yarn or pnpm install"},
	{names: []string{".next", ".nuxt", ".parcel-cache", ".turbo", ".svelte-kit", ".angular"},
		markers: []string{"package.json"},
		title:   "JavaScript build caches", safety: Safe,
		note: "rebuilt on the next build"},
	{names: []string{"target"}, markers: []string{"Cargo.toml"},
		title: "Rust build output (target)", safety: Safe,
		note: "rebuilt by cargo build"},
	{names: []string{".build"}, markers: []string{"Package.swift"},
		title: "Swift build output (.build)", safety: Safe,
		note: "rebuilt by swift build"},
	{names: []string{"build", ".gradle"},
		markers: []string{"build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts"},
		title:   "Gradle build output", safety: Safe,
		note: "rebuilt on the next build"},
	{names: []string{"Pods"}, markers: []string{"Podfile"},
		title: "CocoaPods (Pods)", safety: Safe,
		note: "reinstalled by pod install"},
	{names: []string{".terraform"},
		title: "Terraform providers and modules (.terraform)", safety: Safe,
		note: "re-downloaded by terraform init"},
	{names: []string{"__pycache__"},
		title: "Python bytecode caches (__pycache__)", safety: Safe,
		note: "regenerated automatically"},
	{names: []string{".pytest_cache", ".mypy_cache", ".ruff_cache", ".tox", ".nox"},
		title: "Python tool caches", safety: Safe,
		note: "regenerated on the next run"},
	{names: []string{".venv", "venv", "env"}, markers: []string{"pyvenv.cfg"}, inside: true,
		title: "Python virtual environments", safety: Review,
		note: "recreate with pip, uv or poetry install; check nothing was installed by hand"},
}

// Name rules never fire, and old files are never suggested, inside these.
// They belong to the operating system, installed software, or applications'
// own data, where a folder called "build" or an untouched 2 GB file is
// normal and not the user's to delete. A home directory's dot-folders and
// app bundles (*.app) are excluded the same way, by the matcher. Location
// rules still apply inside zones: that is how ~/Library/Caches is found.
func platformZones() []string {
	return append(platformSystemZones(), homeZones...)
}

// Software installed into a home directory rather than system-wide.
var homeZones = []string{"~/anaconda3", "~/miniconda3", "~/miniforge3", "~/opt", "~/go/pkg"}

func platformSystemZones() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{"/System", "/Library", "/Applications", "/private", "/usr", "/bin", "/sbin",
			"/opt", "/cores", "~/Library"}
	case "windows":
		sysRoot := windowsRoot()
		drive := filepath.VolumeName(sysRoot) + `\`
		return []string{sysRoot, filepath.Join(drive, "Program Files"), filepath.Join(drive, "Program Files (x86)"),
			filepath.Join(drive, "ProgramData"), filepath.Join(drive, "$Recycle.Bin"), "~/AppData"}
	}
	return []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/opt", "/var",
		"/etc", "/boot", "/snap", "/nix", "/proc", "/sys", "/run"}
}

func platformLocRules() []locRule {
	rules := append([]locRule(nil), commonLocRules...)
	switch runtime.GOOS {
	case "darwin":
		rules = append(rules, unixLocRules...)
		rules = append(rules, darwinLocRules...)
	case "linux":
		rules = append(rules, unixLocRules...)
		rules = append(rules, linuxLocRules...)
	case "windows":
		rules = append(rules, windowsLocRules()...)
	}
	return rules
}

// homeDirs lists every home directory on the machine, so that running under
// sudo, or scanning / as root, still recognises each user's caches.
func homeDirs() (own string, all []string) {
	own, _ = os.UserHomeDir()
	if own != "" {
		all = append(all, own)
	}
	var parents []string
	switch runtime.GOOS {
	case "darwin":
		parents = []string{"/Users"}
	case "windows":
		parents = []string{filepath.Join(filepath.VolumeName(windowsRoot())+`\`, "Users")}
	default:
		parents = []string{"/home"}
		all = append(all, "/root")
	}
	skip := map[string]bool{"Shared": true, "Public": true, "Default": true, "Default User": true, "All Users": true}
	for _, p := range parents {
		entries, err := os.ReadDir(p)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() && !skip[e.Name()] {
				all = append(all, filepath.Join(p, e.Name()))
			}
		}
	}
	return own, all
}

func windowsRoot() string {
	if r := os.Getenv("SystemRoot"); r != "" {
		return r
	}
	return `C:\Windows`
}
