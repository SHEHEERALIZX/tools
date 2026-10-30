// branchcheck - report merge status of all branches against a base branch (default: develop).
//
// For every branch it reports: last commit timestamp, date, author, message,
// commits ahead/behind the base branch, and whether it is merged into the base.
//
// Usage examples:
//
//	go run . 
//	go run . -repo /path/to/repo -base develop -format json -out-dir ./reports
//	go run . -exclude "main,master,release/*,hotfix/*" -pending-only
//	go run . -exclude-file .branchcheck-ignore -scope remote -fetch
package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// BranchInfo is one row of the report.
type BranchInfo struct {
	Branch              string `json:"branch"`
	Type                string `json:"type"` // local | remote
	LastCommitHash      string `json:"last_commit_hash"`
	LastCommitTimestamp string `json:"last_commit_timestamp"`
	LastCommitDate      string `json:"last_commit_date"`
	LastCommitAuthor    string `json:"last_commit_author"`
	LastCommitMessage   string `json:"last_commit_message"`
	Ahead               int    `json:"ahead"`  // commits in branch not in base (pending merge)
	Behind              int    `json:"behind"` // commits in base not in branch
	MergedIntoBase      bool   `json:"merged_into_base"`
	Status              string `json:"status"` // merged | pending
}

// Report is the JSON wrapper.
type Report struct {
	Repository  string       `json:"repository"`
	BaseBranch  string       `json:"base_branch"`
	BaseRef     string       `json:"base_ref"`
	GeneratedAt string       `json:"generated_at"`
	Total       int          `json:"total"`
	Pending     int          `json:"pending"`
	Merged      int          `json:"merged"`
	Branches    []BranchInfo `json:"branches"`
}

type config struct {
	repoPath    string
	base        string
	format      string
	outDir      string
	excludeList string
	excludeFile string
	scope       string
	pendingOnly bool
	fetch       bool
}

func main() {
	cfg := parseFlags()

	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func parseFlags() config {
	var cfg config
	flag.StringVar(&cfg.repoPath, "repo", "", "path to the git repository (default: current directory)")
	flag.StringVar(&cfg.base, "base", "develop", "base branch to compare against")
	flag.StringVar(&cfg.format, "format", "csv", "output format: csv or json")
	flag.StringVar(&cfg.outDir, "out-dir", "", "output directory (default: current directory)")
	flag.StringVar(&cfg.excludeList, "exclude", "", "comma-separated branches/glob patterns to exclude, e.g. \"main,release/*\"")
	flag.StringVar(&cfg.excludeFile, "exclude-file", "", "file with one branch/glob pattern per line to exclude (# for comments)")
	flag.StringVar(&cfg.scope, "scope", "all", "which branches to check: local, remote or all")
	flag.BoolVar(&cfg.pendingOnly, "pending-only", false, "only report branches that are NOT yet merged into base")
	flag.BoolVar(&cfg.fetch, "fetch", false, "run 'git fetch --all --prune' before checking")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags]\n\nFlags:\n", filepath.Base(os.Args[0]))
		flag.PrintDefaults()
	}
	flag.Parse()
	return cfg
}

func run(cfg config) error {
	cfg.format = strings.ToLower(strings.TrimSpace(cfg.format))
	if cfg.format != "csv" && cfg.format != "json" {
		return fmt.Errorf("invalid -format %q (use csv or json)", cfg.format)
	}
	cfg.scope = strings.ToLower(strings.TrimSpace(cfg.scope))
	if cfg.scope != "local" && cfg.scope != "remote" && cfg.scope != "all" {
		return fmt.Errorf("invalid -scope %q (use local, remote or all)", cfg.scope)
	}

	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git executable not found in PATH")
	}

	repo, err := resolveRepo(cfg.repoPath)
	if err != nil {
		return err
	}

	excludes, err := buildExcludes(cfg.excludeList, cfg.excludeFile)
	if err != nil {
		return err
	}

	if cfg.fetch {
		fmt.Println("Fetching all remotes...")
		if _, err := git(repo, "fetch", "--all", "--prune"); err != nil {
			return fmt.Errorf("git fetch failed: %w", err)
		}
	}

	baseRef, err := resolveBase(repo, cfg.base)
	if err != nil {
		return err
	}

	branches, err := collectBranches(repo, baseRef, cfg, excludes)
	if err != nil {
		return err
	}

	// Most recently committed first.
	sort.Slice(branches, func(i, j int) bool {
		return branches[i].LastCommitTimestamp > branches[j].LastCommitTimestamp
	})

	report := Report{
		Repository:  repo,
		BaseBranch:  cfg.base,
		BaseRef:     baseRef,
		GeneratedAt: time.Now().Format(time.RFC3339),
		Branches:    branches,
	}
	for _, b := range branches {
		if b.MergedIntoBase {
			report.Merged++
		} else {
			report.Pending++
		}
	}
	report.Total = len(branches)

	outPath, err := writeReport(report, cfg)
	if err != nil {
		return err
	}

	printSummary(report, outPath)
	return nil
}

// resolveRepo returns an absolute repo path and verifies a .git entry exists.
// If no path is given, the current working directory is used.
func resolveRepo(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("cannot determine current directory: %w", err)
		}
		p = cwd
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("invalid repo path %q: %w", p, err)
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("repository path %q does not exist or is not a directory", abs)
	}
	// .git can be a directory (normal repo) or a file (worktree/submodule).
	if _, err := os.Stat(filepath.Join(abs, ".git")); err != nil {
		return "", fmt.Errorf("%q is not a git repository (no .git found); run inside a repo or use -repo", abs)
	}
	return abs, nil
}

// buildExcludes merges patterns from the -exclude list and -exclude-file.
func buildExcludes(list, file string) ([]string, error) {
	var patterns []string
	for _, s := range strings.Split(list, ",") {
		if s = strings.TrimSpace(s); s != "" {
			patterns = append(patterns, s)
		}
	}
	if file != "" {
		f, err := os.Open(file)
		if err != nil {
			return nil, fmt.Errorf("cannot open exclude file: %w", err)
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			patterns = append(patterns, line)
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("reading exclude file: %w", err)
		}
	}
	return patterns, nil
}

// resolveBase finds the base branch locally, falling back to origin/<base>.
func resolveBase(repo, base string) (string, error) {
	candidates := []string{"refs/heads/" + base, "refs/remotes/origin/" + base}
	for _, c := range candidates {
		if _, err := git(repo, "rev-parse", "--verify", "--quiet", c+"^{commit}"); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("base branch %q not found locally or as origin/%s", base, base)
}

func collectBranches(repo, baseRef string, cfg config, excludes []string) ([]BranchInfo, error) {
	var refs []string
	if cfg.scope == "local" || cfg.scope == "all" {
		refs = append(refs, "refs/heads")
	}
	if cfg.scope == "remote" || cfg.scope == "all" {
		refs = append(refs, "refs/remotes")
	}

	format := "%(refname)%1f%(objectname:short)%1f%(committerdate:iso-strict)%1f%(authorname)%1f%(subject)"
	args := append([]string{"for-each-ref", "--format=" + format}, refs...)
	out, err := git(repo, args...)
	if err != nil {
		return nil, fmt.Errorf("listing branches: %w", err)
	}

	var result []BranchInfo
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\x1f")
		if len(f) < 5 {
			continue
		}
		fullRef, hash, ts, author, msg := f[0], f[1], f[2], f[3], f[4]

		var name, typ, bare string
		switch {
		case strings.HasPrefix(fullRef, "refs/heads/"):
			name = strings.TrimPrefix(fullRef, "refs/heads/")
			bare, typ = name, "local"
		case strings.HasPrefix(fullRef, "refs/remotes/"):
			name = strings.TrimPrefix(fullRef, "refs/remotes/")
			typ = "remote"
			bare = name
			if i := strings.Index(name, "/"); i >= 0 {
				bare = name[i+1:]
			}
			if bare == "HEAD" { // origin/HEAD pointer
				continue
			}
		default:
			continue
		}

		// Skip the base branch itself (local and any remote copy).
		if fullRef == baseRef || bare == cfg.base {
			continue
		}
		if isExcluded(excludes, name, bare) {
			continue
		}

		ahead, behind, err := aheadBehind(repo, baseRef, fullRef)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: skipping %s: %v\n", name, err)
			continue
		}
		merged := ahead == 0
		if cfg.pendingOnly && merged {
			continue
		}

		date := ""
		if t, perr := time.Parse(time.RFC3339, ts); perr == nil {
			date = t.Format("2006-01-02")
		} else if len(ts) >= 10 {
			date = ts[:10]
		}

		status := "pending"
		if merged {
			status = "merged"
		}

		result = append(result, BranchInfo{
			Branch:              name,
			Type:                typ,
			LastCommitHash:      hash,
			LastCommitTimestamp: ts,
			LastCommitDate:      date,
			LastCommitAuthor:    author,
			LastCommitMessage:   msg,
			Ahead:               ahead,
			Behind:              behind,
			MergedIntoBase:      merged,
			Status:              status,
		})
	}
	return result, nil
}

// aheadBehind returns commits the branch has that base lacks (ahead),
// and commits base has that the branch lacks (behind).
func aheadBehind(repo, baseRef, branchRef string) (ahead, behind int, err error) {
	out, err := git(repo, "rev-list", "--left-right", "--count", baseRef+"..."+branchRef)
	if err != nil {
		return 0, 0, err
	}
	parts := strings.Fields(out)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("unexpected rev-list output %q", out)
	}
	behind, err = strconv.Atoi(parts[0]) // left side: only in base
	if err != nil {
		return 0, 0, err
	}
	ahead, err = strconv.Atoi(parts[1]) // right side: only in branch
	if err != nil {
		return 0, 0, err
	}
	return ahead, behind, nil
}

// isExcluded matches exact names or glob patterns against the full name
// (e.g. origin/feature/x) and the bare name (feature/x).
func isExcluded(patterns []string, full, bare string) bool {
	for _, p := range patterns {
		for _, candidate := range []string{full, bare} {
			if p == candidate {
				return true
			}
			if ok, _ := path.Match(p, candidate); ok {
				return true
			}
			// Allow trailing "/*" style prefix to match nested names (release/* -> release/1.0/rc).
			if strings.HasSuffix(p, "/*") && strings.HasPrefix(candidate, strings.TrimSuffix(p, "*")) {
				return true
			}
		}
	}
	return false
}

func writeReport(r Report, cfg config) (string, error) {
	dir := cfg.outDir
	if strings.TrimSpace(dir) == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		dir = cwd
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("cannot create output directory: %w", err)
	}

	name := fmt.Sprintf("branch_report_%s_%s.%s", sanitize(cfg.base), time.Now().Format("20060102_150405"), cfg.format)
	full := filepath.Join(dir, name)

	f, err := os.Create(full)
	if err != nil {
		return "", fmt.Errorf("cannot create output file: %w", err)
	}
	defer f.Close()

	switch cfg.format {
	case "json":
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		if err := enc.Encode(r); err != nil {
			return "", err
		}
	default:
		w := csv.NewWriter(f)
		header := []string{
			"branch", "type", "last_commit_hash", "last_commit_timestamp", "last_commit_date",
			"last_commit_author", "last_commit_message", "ahead", "behind", "merged_into_base", "status",
		}
		if err := w.Write(header); err != nil {
			return "", err
		}
		for _, b := range r.Branches {
			row := []string{
				b.Branch, b.Type, b.LastCommitHash, b.LastCommitTimestamp, b.LastCommitDate,
				b.LastCommitAuthor, b.LastCommitMessage,
				strconv.Itoa(b.Ahead), strconv.Itoa(b.Behind),
				strconv.FormatBool(b.MergedIntoBase), b.Status,
			}
			if err := w.Write(row); err != nil {
				return "", err
			}
		}
		w.Flush()
		if err := w.Error(); err != nil {
			return "", err
		}
	}
	return full, nil
}

func printSummary(r Report, outPath string) {
	fmt.Printf("Repository : %s\n", r.Repository)
	fmt.Printf("Base branch: %s (%s)\n", r.BaseBranch, r.BaseRef)
	fmt.Printf("Branches   : %d total | %d pending merge | %d merged\n", r.Total, r.Pending, r.Merged)
	fmt.Printf("Report     : %s\n", outPath)
}

func sanitize(s string) string {
	return strings.NewReplacer("/", "_", "\\", "_", " ", "_").Replace(s)
}

// git runs a git command in repo and returns trimmed stdout.
func git(repo string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(string(out)), nil
}
