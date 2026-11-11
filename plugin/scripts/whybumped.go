// Command whybumped compares the Go module dependency graph of two revisions
// of a module and explains every change in the selected versions: which
// dependency required the new version, through which chain it reached the
// module, and which dependencies forced the go directive up.
//
// It runs `go mod graph` and `go list -m -json all` on each side and never
// builds or runs the module's code.
package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const version = "0.1.0"

// Exit codes.
const (
	exitOK        = 0
	exitFindings  = 1
	exitUntrusted = 2
)

// Severities, lowest first.
const (
	sevInfo = "INFO"
	sevWarn = "WARN"
)

// module is one entry of `go list -m -json all`.
type module struct {
	Path      string
	Version   string
	Main      bool
	Indirect  bool
	GoVersion string
	Replace   *module
	Error     *struct{ Err string }
}

// side is one revision of the module, fully loaded.
type side struct {
	Label     string
	Dir       string
	Main      string
	Go        string // go directive of the main module
	Toolchain string // toolchain directive, if any
	Selected  map[string]module
	// edges maps a node ("path@version", or the main module path) to the
	// nodes it requires. go@ and toolchain@ nodes are dropped.
	Edges map[string][]string
	// requirers maps a node to the nodes that require it, the main module
	// included.
	Requirers map[string][]string
}

// Finding is one change in the report.
type Finding struct {
	Severity string   `json:"severity"`
	Change   string   `json:"change"`
	Module   string   `json:"module"`
	Old      string   `json:"old,omitempty"`
	New      string   `json:"new,omitempty"`
	Cause    string   `json:"cause,omitempty"`
	Chain    []string `json:"chain,omitempty"`
	Others   []string `json:"others,omitempty"`
	Detail   string   `json:"detail"`
}

// Report is the whole result, also the JSON output.
type Report struct {
	Tool      string    `json:"tool"`
	Version   string    `json:"version"`
	Old       string    `json:"old"`
	New       string    `json:"new"`
	Module    string    `json:"module"`
	OldCount  int       `json:"old_modules"`
	NewCount  int       `json:"new_modules"`
	Unchanged int       `json:"unchanged"`
	Findings  []Finding `json:"findings"`
	Roots     []Root    `json:"roots"`
}

// Root is a module that go.mod requires directly and that, through its own
// requirements, caused one or more of the module findings.
type Root struct {
	Node    string `json:"node"`
	Changes int    `json:"changes"`
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("whybumped", flag.ContinueOnError)
	fs.SetOutput(stderr)
	oldDir := fs.String("old", "", "module directory of the old revision")
	base := fs.String("base", "", "git ref of the old revision, read from the repository of --new")
	newDir := fs.String("new", ".", "module directory of the new revision")
	format := fs.String("format", "text", "output format: text or json")
	failOn := fs.String("fail-on", "warn", "lowest severity that exits 1: warn or never")
	showVersion := fs.Bool("version", false, "print the version and exit")
	var expects multiFlag
	fs.Var(&expects, "expect", "module change you intend, as PATH or PATH@VERSION (repeatable)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: whybumped (--old DIR | --base GITREF) [--new DIR] [--expect PATH[@VERSION]]... [--format text|json] [--fail-on warn|never]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUntrusted
	}
	if *showVersion {
		fmt.Fprintln(stdout, "whybumped", version)
		return exitOK
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "whybumped: unexpected argument %q\n", fs.Arg(0))
		return exitUntrusted
	}
	if (*oldDir == "") == (*base == "") {
		fmt.Fprintln(stderr, "whybumped: give exactly one of --old DIR or --base GITREF")
		return exitUntrusted
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "whybumped: --format must be text or json, not %q\n", *format)
		return exitUntrusted
	}
	if *failOn != "warn" && *failOn != "never" {
		fmt.Fprintf(stderr, "whybumped: --fail-on must be warn or never, not %q\n", *failOn)
		return exitUntrusted
	}
	exp, err := parseExpects(expects)
	if err != nil {
		fmt.Fprintln(stderr, "whybumped:", err)
		return exitUntrusted
	}

	oldLabel := *oldDir
	oldPath := *oldDir
	if *base != "" {
		tmp, err := os.MkdirTemp("", "whybumped-base-")
		if err != nil {
			fmt.Fprintln(stderr, "whybumped:", err)
			return exitUntrusted
		}
		defer os.RemoveAll(tmp)
		oldPath, err = extractGitRev(*newDir, *base, tmp)
		if err != nil {
			fmt.Fprintln(stderr, "whybumped: --base:", err)
			return exitUntrusted
		}
		oldLabel = *base
	}

	oldSide, err := loadSide(oldLabel, oldPath)
	if err != nil {
		fmt.Fprintf(stderr, "whybumped: old side %s: %v\n", oldLabel, err)
		return exitUntrusted
	}
	newSide, err := loadSide(*newDir, *newDir)
	if err != nil {
		fmt.Fprintf(stderr, "whybumped: new side %s: %v\n", *newDir, err)
		return exitUntrusted
	}
	if oldSide.Main != newSide.Main {
		fmt.Fprintf(stderr, "whybumped: the two sides are different modules: %s and %s\n", oldSide.Main, newSide.Main)
		return exitUntrusted
	}
	if len(oldSide.Selected) == 0 && len(newSide.Selected) == 0 {
		fmt.Fprintf(stderr, "whybumped: neither side has any dependency, so there is nothing to compare\n")
		return exitUntrusted
	}
	if len(oldSide.Selected) == 0 || len(newSide.Selected) == 0 {
		empty := oldSide
		if len(newSide.Selected) == 0 {
			empty = newSide
		}
		fmt.Fprintf(stderr, "whybumped: %s has no dependencies while the other side has some; check that its go.mod is complete\n", empty.Label)
		return exitUntrusted
	}

	rep := compare(oldSide, newSide, exp)
	if *format == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return exitUntrusted
		}
	} else {
		writeText(stdout, rep)
	}
	if *failOn == "never" {
		return exitOK
	}
	for _, f := range rep.Findings {
		if f.Severity == sevWarn {
			return exitFindings
		}
	}
	return exitOK
}

// expectation is one --expect flag.
type expectation struct {
	Path    string
	Version string // empty means any change
}

func parseExpects(list []string) ([]expectation, error) {
	var out []expectation
	for _, e := range list {
		p, v, _ := strings.Cut(e, "@")
		if p == "" || strings.ContainsAny(p, " \t") {
			return nil, fmt.Errorf("--expect %q: want PATH or PATH@VERSION", e)
		}
		if v != "" {
			if _, ok := parseSemver(v); !ok {
				return nil, fmt.Errorf("--expect %q: %q is not a module version such as v1.2.3", e, v)
			}
		}
		out = append(out, expectation{Path: p, Version: v})
	}
	return out, nil
}

// ---- loading ----

func goCmd(dir string, args ...string) (stdout, stderr []byte, err error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	var o, e bytes.Buffer
	cmd.Stdout = &o
	cmd.Stderr = &e
	err = cmd.Run()
	return o.Bytes(), e.Bytes(), err
}

func tail(b []byte) string {
	s := strings.TrimSpace(string(b))
	lines := strings.Split(s, "\n")
	if len(lines) > 6 {
		lines = lines[len(lines)-6:]
	}
	return strings.Join(lines, "\n  ")
}

// checkGoMod reads go.mod as text before the go command does, to catch a
// file the go command would accept but that can't be complete.
func checkGoMod(dir string) (modPath, goLine, toolchain string, err error) {
	st, err := os.Stat(dir)
	if err != nil {
		return "", "", "", fmt.Errorf("can't read directory: %v", err)
	}
	if !st.IsDir() {
		return "", "", "", errors.New("not a directory")
	}
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return "", "", "", fmt.Errorf("no readable go.mod: %v", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return "", "", "", errors.New("go.mod is empty")
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return "", "", "", errors.New("go.mod is not UTF-8 text")
	}
	if data[len(data)-1] != '\n' {
		return "", "", "", errors.New("go.mod doesn't end with a newline, so it may be cut off (the go command always writes one)")
	}
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		f := strings.Fields(line)
		if len(f) == 2 {
			switch f[0] {
			case "module":
				modPath = strings.Trim(f[1], `"`)
			case "go":
				goLine = f[1]
			case "toolchain":
				toolchain = f[1]
			}
		}
	}
	if modPath == "" {
		return "", "", "", errors.New("go.mod has no module line")
	}
	if goLine == "" {
		return "", "", "", errors.New("go.mod has no go directive, so it may be cut off or very old; add one with `go mod edit -go=VERSION`")
	}
	return modPath, goLine, toolchain, nil
}

func loadSide(label, dir string) (*side, error) {
	modPath, goLine, toolchain, err := checkGoMod(dir)
	if err != nil {
		return nil, err
	}
	s := &side{
		Label:     label,
		Dir:       dir,
		Toolchain: toolchain,
		Selected:  map[string]module{},
		Edges:     map[string][]string{},
		Requirers: map[string][]string{},
	}

	out, errOut, err := goCmd(dir, "mod", "graph")
	if err != nil {
		return nil, fmt.Errorf("go mod graph failed: %v\n  %s", err, tail(errOut))
	}
	if err := s.parseGraph(out); err != nil {
		return nil, fmt.Errorf("go mod graph output can't be used: %v", err)
	}

	out, errOut, err = goCmd(dir, "list", "-m", "-mod=readonly", "-json", "all")
	if err != nil {
		return nil, fmt.Errorf("go list -m all failed: %v\n  %s", err, tail(errOut))
	}
	mods, err := parseModules(out)
	if err != nil {
		return nil, fmt.Errorf("go list -m output can't be used: %v", err)
	}
	mains := 0
	for _, m := range mods {
		if m.Main {
			mains++
			s.Main = m.Path
			s.Go = m.GoVersion
			continue
		}
		s.Selected[m.Path] = m
	}
	if mains != 1 {
		return nil, fmt.Errorf("go list -m reported %d main modules, want 1", mains)
	}
	if s.Main != modPath {
		return nil, fmt.Errorf("go list -m reports main module %s but go.mod says %s", s.Main, modPath)
	}
	if s.Go == "" {
		s.Go = goLine
	}
	if _, ok := parseGoVersion(s.Go); !ok {
		return nil, fmt.Errorf("go directive %q is not a Go version", s.Go)
	}
	if err := s.crossCheck(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *side) parseGraph(out []byte) error {
	if len(bytes.TrimSpace(out)) == 0 {
		return errors.New("empty output")
	}
	if !utf8.Valid(out) {
		return errors.New("not UTF-8 text")
	}
	if out[len(out)-1] != '\n' {
		return errors.New("output doesn't end with a newline, so it may be cut off")
	}
	mains := map[string]bool{}
	seen := map[[2]string]bool{}
	for i, line := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			return fmt.Errorf("line %d isn't two fields: %q", i+1, clip(line))
		}
		from, to := f[0], f[1]
		fp, fv, fok := strings.Cut(from, "@")
		tp, tv, tok := strings.Cut(to, "@")
		if !tok {
			return fmt.Errorf("line %d: %q has no version", i+1, clip(to))
		}
		if isGoNode(fp) || isGoNode(tp) {
			okFrom := isGoNode(fp) && validGoNode(from) || !isGoNode(fp) && (!fok && validPath(from) || fok && validPath(fp) && validVersion(fv))
			if !isGoNode(tp) || !validGoNode(to) || !okFrom {
				return fmt.Errorf("line %d: bad go or toolchain node: %q", i+1, clip(line))
			}
			if !fok {
				mains[from] = true
			}
			continue
		}
		if !fok {
			if !validPath(from) {
				return fmt.Errorf("line %d: %q isn't a module path", i+1, clip(from))
			}
			mains[from] = true
		} else if !validPath(fp) || !validVersion(fv) {
			return fmt.Errorf("line %d: %q isn't path@version", i+1, clip(from))
		}
		if !validPath(tp) || !validVersion(tv) {
			return fmt.Errorf("line %d: %q isn't path@version", i+1, clip(to))
		}
		k := [2]string{from, to}
		if seen[k] {
			continue
		}
		seen[k] = true
		s.Edges[from] = append(s.Edges[from], to)
		s.Requirers[to] = append(s.Requirers[to], from)
	}
	if len(mains) > 1 {
		return fmt.Errorf("%d main modules in the graph; whybumped runs with GOWORK=off and expects one", len(mains))
	}
	return nil
}

func isGoNode(p string) bool { return p == "go" || p == "toolchain" }

func validGoNode(n string) bool {
	p, v, ok := strings.Cut(n, "@")
	if !ok {
		return false
	}
	if p == "toolchain" {
		v = strings.TrimPrefix(v, "go")
		v, _, _ = strings.Cut(v, "-")
	}
	_, good := parseGoVersion(v)
	return good
}

func validPath(p string) bool {
	if p == "" || len(p) > 500 {
		return false
	}
	for _, r := range p {
		if r <= ' ' || r == '@' || r == '"' || r == '\'' || r == '`' || r == '\\' || r == 0x7f {
			return false
		}
	}
	return true
}

func validVersion(v string) bool {
	_, ok := parseSemver(v)
	return ok
}

func clip(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

func parseModules(out []byte) ([]module, error) {
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, errors.New("empty output")
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	var mods []module
	for {
		var m module
		err := dec.Decode(&m)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("bad JSON: %v", err)
		}
		if m.Error != nil {
			return nil, fmt.Errorf("module %s: %s", m.Path, m.Error.Err)
		}
		if m.Path == "" {
			return nil, errors.New("a module has no path")
		}
		if !m.Main && !validVersion(m.Version) {
			return nil, fmt.Errorf("module %s has no valid version (%q)", m.Path, m.Version)
		}
		mods = append(mods, m)
	}
	return mods, nil
}

// crossCheck computes minimal version selection from the graph and compares
// it with what `go list -m all` selected. A mismatch means one of the two
// outputs is incomplete or the tool's reading of it is wrong.
func (s *side) crossCheck() error {
	if len(s.Edges[s.Main]) == 0 && len(s.Selected) > 0 {
		return errors.New("go mod graph has no requirements of the main module, but go list -m lists dependencies")
	}
	fromGraph := map[string]string{}
	for node := range s.reachable() {
		if node == s.Main {
			continue
		}
		p, v, _ := strings.Cut(node, "@")
		if cur, ok := fromGraph[p]; !ok || compareSemver(v, cur) > 0 {
			fromGraph[p] = v
		}
	}
	for p, v := range fromGraph {
		m, ok := s.Selected[p]
		if !ok {
			return fmt.Errorf("go mod graph has %s@%s but go list -m all doesn't list %s", p, v, p)
		}
		if m.Version != v {
			return fmt.Errorf("go mod graph selects %s@%s but go list -m all selects %s", p, v, m.Version)
		}
	}
	for p, m := range s.Selected {
		if _, ok := fromGraph[p]; !ok {
			return fmt.Errorf("go list -m all lists %s@%s but go mod graph doesn't reach it", p, m.Version)
		}
	}
	return nil
}

func (s *side) reachable() map[string]bool {
	seen := map[string]bool{s.Main: true}
	queue := []string{s.Main}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, t := range s.Edges[n] {
			if !seen[t] {
				seen[t] = true
				queue = append(queue, t)
			}
		}
	}
	return seen
}

// ---- git ----

func extractGitRev(newDir, ref, dest string) (string, error) {
	if strings.HasPrefix(ref, "-") {
		return "", fmt.Errorf("ref %q looks like a flag", ref)
	}
	git := func(args ...string) ([]byte, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = newDir
		var o, e bytes.Buffer
		cmd.Stdout = &o
		cmd.Stderr = &e
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(e.String()))
		}
		return o.Bytes(), nil
	}
	top, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	prefix, err := git("rev-parse", "--show-prefix")
	if err != nil {
		return "", err
	}
	if _, err := git("rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}"); err != nil {
		return "", fmt.Errorf("%q is not a commit in this repository", ref)
	}
	cmd := exec.Command("git", "archive", "--format=tar", ref, "--",
		":(glob)**/go.mod", ":(glob)**/go.sum")
	cmd.Dir = strings.TrimSpace(string(top))
	var e bytes.Buffer
	cmd.Stderr = &e
	tarData, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git archive: %v: %s", err, strings.TrimSpace(e.String()))
	}
	if err := untar(tarData, dest); err != nil {
		return "", err
	}
	dir := filepath.Join(dest, filepath.FromSlash(strings.TrimSpace(string(prefix))))
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return "", fmt.Errorf("%s has no go.mod at %s", ref, strings.TrimSpace(string(prefix))+"go.mod")
	}
	return dir, nil
}

// untar extracts regular files and directories, refusing any path that
// would leave dest. Links and other entry types are skipped.
func untar(data []byte, dest string) error {
	tr := tar.NewReader(bytes.NewReader(data))
	n := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("archive is damaged: %v", err)
		}
		name := filepath.FromSlash(h.Name)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) || strings.Contains(name, string(filepath.Separator)+".."+string(filepath.Separator)) {
			return fmt.Errorf("archive path %q leaves the target directory", h.Name)
		}
		target := filepath.Join(dest, name)
		rel, err := filepath.Rel(dest, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("archive path %q leaves the target directory", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			cerr := f.Close()
			if err != nil {
				return fmt.Errorf("archive is damaged: %v", err)
			}
			if cerr != nil {
				return cerr
			}
			n++
		}
	}
	if n == 0 {
		return errors.New("archive has no go.mod or go.sum files")
	}
	return nil
}

// ---- comparison ----

func compare(o, n *side, exp []expectation) Report {
	rep := Report{
		Tool:     "whybumped",
		Version:  version,
		Old:      o.Label,
		New:      n.Label,
		Module:   n.Main,
		OldCount: len(o.Selected),
		NewCount: len(n.Selected),
	}
	expected := map[string]expectation{}
	for _, e := range exp {
		expected[e.Path] = e
	}
	strict := len(exp) > 0

	var fs []Finding
	if f, ok := goDirective(o, n); ok {
		fs = append(fs, f)
	}
	if o.Toolchain != n.Toolchain {
		fs = append(fs, Finding{
			Severity: sevInfo, Change: "toolchain-changed", Module: "toolchain",
			Old: o.Toolchain, New: n.Toolchain,
			Detail: fmt.Sprintf("toolchain line %s -> %s; it only picks the Go toolchain for work in this module", orNone(o.Toolchain), orNone(n.Toolchain)),
		})
	}

	paths := map[string]bool{}
	for p := range o.Selected {
		paths[p] = true
	}
	for p := range n.Selected {
		paths[p] = true
	}
	var mods []Finding
	for p := range paths {
		om, inOld := o.Selected[p]
		nm, inNew := n.Selected[p]
		switch {
		case inOld && inNew && om.Version == nm.Version:
			rep.Unchanged++
			continue
		case inOld && inNew && compareSemver(nm.Version, om.Version) > 0:
			mods = append(mods, explainUp(n, p, om.Version, nm, "raised"))
		case inOld && inNew:
			mods = append(mods, explainDown(o, n, p, om.Version, nm.Version))
		case inNew:
			mods = append(mods, explainUp(n, p, "", nm, "added"))
		default:
			mods = append(mods, explainRemoved(o, p, om.Version))
		}
	}
	mods = append(mods, requirementLines(o, n)...)
	for i := range mods {
		f := &mods[i]
		e, isExpected := expected[f.Module]
		switch {
		case isExpected && (e.Version == "" || e.Version == f.New):
			f.Severity = sevInfo
			f.Detail = "expected; " + f.Detail
			delete(expected, f.Module)
		case isExpected:
			f.Severity = sevWarn
			f.Detail = fmt.Sprintf("expected %s but got %s; %s", e.Version, orNone(f.New), f.Detail)
			delete(expected, f.Module)
		case strict && f.Cause == "requested":
			f.Severity = sevWarn
			f.Detail = "not in --expect; " + f.Detail
		}
	}
	sort.Slice(mods, func(i, j int) bool {
		if mods[i].Severity != mods[j].Severity {
			return mods[i].Severity == sevWarn
		}
		return mods[i].Module < mods[j].Module
	})
	fs = append(fs, mods...)

	var missed []string
	for p := range expected {
		missed = append(missed, p)
	}
	sort.Strings(missed)
	for _, p := range missed {
		e := expected[p]
		cur := "not required"
		if m, ok := n.Selected[p]; ok {
			cur = m.Version
		}
		want := "a change"
		if e.Version != "" {
			want = e.Version
		}
		fs = append(fs, Finding{
			Severity: sevWarn, Change: "expect-missed", Module: p, New: n.Selected[p].Version,
			Detail: fmt.Sprintf("expected %s, but the selected version didn't change (%s)", want, cur),
		})
	}
	rep.Findings = fs
	if rep.Findings == nil {
		rep.Findings = []Finding{}
	}
	rep.Roots = roots(mods)
	return rep
}

// requirementLines reports modules whose line in go.mod was added or removed
// while the selected version stayed the same. A changed selection is already
// a module finding, so only these would otherwise go unseen.
func requirementLines(o, n *side) []Finding {
	direct := func(s *side) map[string]string {
		m := map[string]string{}
		for _, node := range s.Edges[s.Main] {
			p, v, _ := strings.Cut(node, "@")
			m[p] = v
		}
		return m
	}
	or, nr := direct(o), direct(n)
	var out []Finding
	for p, v := range or {
		if _, ok := nr[p]; ok {
			continue
		}
		om, inOld := o.Selected[p]
		nm, inNew := n.Selected[p]
		if !inOld || !inNew || om.Version != nm.Version {
			continue
		}
		f := Finding{Severity: sevWarn, Change: "requirement-dropped", Module: p, Old: v, Cause: "pulled"}
		f.Chain = chainFor(n, p+"@"+nm.Version)
		f.Detail = fmt.Sprintf("go.mod no longer lists %s %s; it is still selected at %s", p, v, nm.Version)
		if len(f.Chain) > 1 {
			f.Detail += " because " + f.Chain[1] + " requires it"
		}
		f.Detail += "; `go mod tidy` drops a line when no package needs the module, and a go.mod that lost lines looks the same"
		out = append(out, f)
	}
	for p, v := range nr {
		if _, ok := or[p]; ok {
			continue
		}
		om, inOld := o.Selected[p]
		nm, inNew := n.Selected[p]
		if !inOld || !inNew || om.Version != nm.Version {
			continue
		}
		out = append(out, Finding{Severity: sevInfo, Change: "requirement-added", Module: p, New: v, Cause: "requested",
			Detail: fmt.Sprintf("go.mod now lists %s %s; the selected version didn't change", p, v)})
	}
	return out
}

// roots counts, for each module at the end of a "pulled" chain, how many
// module findings it explains.
func roots(mods []Finding) []Root {
	count := map[string]int{}
	for _, f := range mods {
		if f.Cause == "pulled" && len(f.Chain) > 1 && f.Change != "requirement-dropped" {
			count[f.Chain[len(f.Chain)-1]]++
		}
	}
	out := []Root{}
	for n, c := range count {
		out = append(out, Root{Node: n, Changes: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Changes != out[j].Changes {
			return out[i].Changes > out[j].Changes
		}
		return out[i].Node < out[j].Node
	})
	return out
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// goDirective reports a change of the main module's go line and, when it
// went up, which selected dependencies need more than the old line.
func goDirective(o, n *side) (Finding, bool) {
	c := compareGo(n.Go, o.Go)
	if c == 0 {
		return Finding{}, false
	}
	f := Finding{Module: "go", Old: o.Go, New: n.Go}
	if sameRelease(o.Go, n.Go) {
		f.Severity = sevInfo
		f.Change = "go-respelled"
		f.Detail = fmt.Sprintf("go directive rewritten from %s to %s, which names the same first release; newer go commands write the three-part form", o.Go, n.Go)
		return f, true
	}
	if c < 0 {
		f.Severity = sevInfo
		f.Change = "go-lowered"
		f.Detail = fmt.Sprintf("go directive lowered from %s to %s", o.Go, n.Go)
		return f, true
	}
	f.Severity = sevWarn
	f.Change = "go-raised"
	type need struct {
		path, ver, gov string
		depth          int
	}
	var needs []need
	maxDep := ""
	for p, m := range n.Selected {
		if m.GoVersion == "" {
			continue
		}
		if maxDep == "" || compareGo(m.GoVersion, maxDep) > 0 {
			maxDep = m.GoVersion
		}
		if compareGo(m.GoVersion, o.Go) > 0 && !sameRelease(m.GoVersion, o.Go) {
			needs = append(needs, need{p, m.Version, m.GoVersion, len(chainFor(n, p+"@"+m.Version))})
		}
	}
	sort.Slice(needs, func(i, j int) bool {
		if c := compareGo(needs[i].gov, needs[j].gov); c != 0 {
			return c > 0
		}
		if needs[i].depth != needs[j].depth {
			return needs[i].depth < needs[j].depth
		}
		return needs[i].path < needs[j].path
	})
	if len(needs) == 0 {
		f.Cause = "requested"
		f.Detail = fmt.Sprintf("go directive raised from %s to %s by go.mod itself; no selected dependency needs more than go %s", o.Go, n.Go, orNone(maxDep))
		return f, true
	}
	top := needs[0]
	f.Cause = "pulled"
	f.Chain = chainFor(n, top.path+"@"+top.ver)
	for _, x := range needs[1:] {
		if len(f.Others) == 5 {
			f.Others = append(f.Others, fmt.Sprintf("%d more", len(needs)-6))
			break
		}
		f.Others = append(f.Others, fmt.Sprintf("%s@%s (go %s)", x.path, x.ver, x.gov))
	}
	f.Detail = fmt.Sprintf("go directive raised from %s to %s; %s@%s needs go %s", o.Go, n.Go, top.path, top.ver, top.gov)
	if len(f.Chain) == 1 {
		f.Detail += " and go.mod requires it directly"
	}
	if len(needs) > 1 {
		f.Detail += fmt.Sprintf(", and %d more selected modules need more than go %s", len(needs)-1, o.Go)
	}
	if compareGo(n.Go, top.gov) > 0 {
		f.Detail += fmt.Sprintf("; go.mod went further than any dependency needs (go %s)", top.gov)
	}
	return f, true
}

// nonMainRequirers returns the nodes other than the main module that
// require node, sorted.
func nonMainRequirers(s *side, node string) []string {
	var out []string
	for _, r := range s.Requirers[node] {
		if r != s.Main {
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}

// chainFor walks up from node through requirers other than the main module,
// breadth first, until it reaches a node that only the main module's go.mod
// requires. It returns that chain, node first. When nothing but go.mod
// requires node, the chain is just node.
func chainFor(s *side, node string) []string {
	parent := map[string]string{node: ""}
	queue := []string{node}
	var last string
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		last = cur
		reqs := nonMainRequirers(s, cur)
		// Prefer requirers whose version is the selected one.
		sort.SliceStable(reqs, func(i, j int) bool {
			return s.isSelected(reqs[i]) && !s.isSelected(reqs[j])
		})
		if len(reqs) == 0 {
			return withReplacements(s, buildChain(parent, cur))
		}
		for _, r := range reqs {
			if _, ok := parent[r]; !ok {
				parent[r] = cur
				queue = append(queue, r)
			}
		}
	}
	return withReplacements(s, buildChain(parent, last))
}

func buildChain(parent map[string]string, end string) []string {
	var rev []string
	for n := end; n != ""; n = parent[n] {
		rev = append(rev, n)
	}
	// rev is end ... node; reverse it so node comes first.
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

// withReplacements annotates each node in a chain with its replacement, when
// the selected module has a replace directive.
func withReplacements(s *side, nodes []string) []string {
	out := make([]string, len(nodes))
	for i, node := range nodes {
		out[i] = nodeWithReplacement(s, node)
	}
	return out
}

func nodeWithReplacement(s *side, node string) string {
	path, version, _ := strings.Cut(node, "@")
	m, ok := s.Selected[path]
	if !ok || m.Version != version || m.Replace == nil {
		return node
	}
	if m.Replace.Version != "" {
		return node + " => " + m.Replace.Path + "@" + m.Replace.Version
	}
	return node + " => " + m.Replace.Path
}

func (s *side) isSelected(node string) bool {
	p, v, _ := strings.Cut(node, "@")
	m, ok := s.Selected[p]
	return ok && m.Version == v
}

// maxNeeded returns the highest version of path that any node other than
// the main module requires, or "".
func maxNeeded(s *side, path string) string {
	best := ""
	for node := range s.Requirers {
		p, v, _ := strings.Cut(node, "@")
		if p != path || len(nonMainRequirers(s, node)) == 0 {
			continue
		}
		if best == "" || compareSemver(v, best) > 0 {
			best = v
		}
	}
	return best
}

func explainUp(n *side, path, oldV string, nm module, change string) Finding {
	node := path + "@" + nm.Version
	f := Finding{Change: change, Module: path, Old: oldV, New: nm.Version}
	reqs := nonMainRequirers(n, node)
	if len(reqs) == 0 {
		f.Severity = sevInfo
		f.Cause = "requested"
		need := maxNeeded(n, path)
		what := "go.mod requires this version and no dependency requires it"
		if need != "" {
			what = fmt.Sprintf("go.mod requires this version; dependencies need at most %s", need)
		} else if change == "added" {
			what = "go.mod requires this module and no dependency requires it"
		}
		if nm.Indirect {
			what += "; it is marked // indirect, so an explicit `go get` or `go get -u` raised it"
		}
		f.Detail = what
		return f
	}
	f.Severity = sevWarn
	f.Cause = "pulled"
	f.Chain = chainFor(n, node)
	if len(f.Chain) > 1 {
		first := f.Chain[1]
		for _, r := range reqs {
			if r != first {
				if len(f.Others) == 5 {
					f.Others = append(f.Others, fmt.Sprintf("%d more", len(reqs)-6))
					break
				}
				f.Others = append(f.Others, r)
			}
		}
	}
	verb := "raised to " + nm.Version
	if change == "added" {
		verb = "added at " + nm.Version
	}
	f.Detail = fmt.Sprintf("%s because %s requires it", verb, f.Chain[1])
	if !n.isSelected(f.Chain[1]) {
		rp, _, _ := strings.Cut(f.Chain[1], "@")
		f.Detail += fmt.Sprintf(" (that version of %s isn't selected, %s is, but its requirements still count)", rp, n.Selected[rp].Version)
	}
	return f
}

func explainDown(o, n *side, path, oldV, newV string) Finding {
	f := Finding{Severity: sevWarn, Change: "lowered", Module: path, Old: oldV, New: newV}
	reqs := nonMainRequirers(o, path+"@"+oldV)
	if len(reqs) == 0 {
		f.Cause = "requested"
		f.Detail = fmt.Sprintf("lowered from %s to %s; before, only go.mod required %s", oldV, newV, oldV)
		return f
	}
	f.Cause = "released"
	f.Chain = chainFor(o, path+"@"+oldV)
	var gone []string
	for _, r := range reqs {
		rp, rv, _ := strings.Cut(r, "@")
		now := "removed"
		if m, ok := n.Selected[rp]; ok {
			now = m.Version
		}
		if now != rv {
			gone = append(gone, fmt.Sprintf("%s (now %s)", r, now))
		}
	}
	f.Others = gone
	f.Detail = fmt.Sprintf("lowered from %s to %s; %s required %s before", oldV, newV, f.Chain[1], oldV)
	return f
}

func explainRemoved(o *side, path, oldV string) Finding {
	f := Finding{Severity: sevInfo, Change: "removed", Module: path, Old: oldV}
	reqs := nonMainRequirers(o, path+"@"+oldV)
	if len(reqs) == 0 {
		// go.mod itself dropped it: a deliberate removal, or a go.mod
		// that lost lines. Either way a reviewer should see it.
		f.Severity = sevWarn
		f.Cause = "requested"
		f.Detail = "no longer required; before, only go.mod required it"
		return f
	}
	f.Cause = "released"
	f.Chain = chainFor(o, path+"@"+oldV)
	f.Detail = fmt.Sprintf("no longer required; %s required it before", f.Chain[1])
	return f
}

// ---- output ----

func writeText(w io.Writer, rep Report) {
	fmt.Fprintf(w, "whybumped %s -> %s: %s, %d modules before, %d after, %d unchanged\n",
		rep.Old, rep.New, rep.Module, rep.OldCount, rep.NewCount, rep.Unchanged)
	warns, infos := 0, 0
	for _, f := range rep.Findings {
		if f.Severity == sevWarn {
			warns++
		} else {
			infos++
		}
		head := f.Module
		switch {
		case f.Old != "" && f.New != "":
			head += " " + f.Old + " -> " + f.New
		case f.New != "":
			head += " " + f.New
		case f.Old != "":
			head += " " + f.Old
		}
		fmt.Fprintf(w, "  %-5s %s %s\n", f.Severity, f.Change, head)
		fmt.Fprintf(w, "        %s\n", f.Detail)
		if len(f.Chain) > 1 {
			fmt.Fprintf(w, "        via %s <- go.mod\n", strings.Join(f.Chain[1:], " <- "))
		}
		if len(f.Others) > 0 {
			label := "also required by"
			switch f.Change {
			case "go-raised":
				label = "also needs more"
			case "lowered":
				label = "requirers that moved"
			}
			fmt.Fprintf(w, "        %s: %s\n", label, strings.Join(f.Others, ", "))
		}
	}
	if len(rep.Roots) > 0 {
		var parts []string
		for i, r := range rep.Roots {
			if i == 5 {
				parts = append(parts, fmt.Sprintf("%d more", len(rep.Roots)-5))
				break
			}
			parts = append(parts, fmt.Sprintf("%s (%d)", r.Node, r.Changes))
		}
		fmt.Fprintf(w, "  pulled in through: %s\n", strings.Join(parts, ", "))
	}
	switch {
	case len(rep.Findings) == 0:
		fmt.Fprintln(w, "  no dependency changes")
	default:
		fmt.Fprintf(w, "  %d %s, %d %s\n", warns, plural(warns, "warning", "warnings"), infos, plural(infos, "info", "info"))
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// ---- versions ----

type semver struct {
	major, minor, patch int64
	pre                 []string
}

// parseSemver parses a Go module version: vMAJOR[.MINOR[.PATCH]][-pre][+build].
// Module versions in go.mod are always the full form, so short forms are
// rejected.
func parseSemver(v string) (semver, bool) {
	var s semver
	if !strings.HasPrefix(v, "v") {
		return s, false
	}
	v = v[1:]
	if i := strings.IndexByte(v, '+'); i >= 0 {
		build := v[i+1:]
		if build == "" || !validIdents(build, false) {
			return s, false
		}
		v = v[:i]
	}
	core, pre, hasPre := strings.Cut(v, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return s, false
	}
	nums := make([]int64, 3)
	for i, p := range parts {
		if p == "" || (len(p) > 1 && p[0] == '0') {
			return s, false
		}
		x, err := strconv.ParseInt(p, 10, 64)
		if err != nil || x < 0 {
			return s, false
		}
		nums[i] = x
	}
	s.major, s.minor, s.patch = nums[0], nums[1], nums[2]
	if hasPre {
		if pre == "" || !validIdents(pre, true) {
			return s, false
		}
		s.pre = strings.Split(pre, ".")
	}
	return s, true
}

func validIdents(s string, noLeadingZero bool) bool {
	for _, id := range strings.Split(s, ".") {
		if id == "" {
			return false
		}
		allDigits := true
		for _, r := range id {
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '-') {
				return false
			}
			if r < '0' || r > '9' {
				allDigits = false
			}
		}
		if noLeadingZero && allDigits && len(id) > 1 && id[0] == '0' {
			return false
		}
	}
	return true
}

// compareSemver orders two module versions. Invalid versions sort first.
func compareSemver(a, b string) int {
	x, okx := parseSemver(a)
	y, oky := parseSemver(b)
	switch {
	case !okx && !oky:
		return strings.Compare(a, b)
	case !okx:
		return -1
	case !oky:
		return 1
	}
	for _, d := range [][2]int64{{x.major, y.major}, {x.minor, y.minor}, {x.patch, y.patch}} {
		if d[0] != d[1] {
			if d[0] < d[1] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(x.pre) == 0 && len(y.pre) == 0:
		return 0
	case len(x.pre) == 0:
		return 1
	case len(y.pre) == 0:
		return -1
	}
	for i := 0; i < len(x.pre) && i < len(y.pre); i++ {
		if c := compareIdent(x.pre[i], y.pre[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(x.pre) < len(y.pre):
		return -1
	case len(x.pre) > len(y.pre):
		return 1
	}
	return 0
}

func compareIdent(a, b string) int {
	an, aerr := strconv.ParseUint(a, 10, 64)
	bn, berr := strconv.ParseUint(b, 10, 64)
	switch {
	case aerr == nil && berr == nil:
		if an < bn {
			return -1
		} else if an > bn {
			return 1
		}
		return 0
	case aerr == nil:
		return -1
	case berr == nil:
		return 1
	}
	return strings.Compare(a, b)
}

// goVersion is a parsed Go release or language version: 1.21, 1.21rc1,
// 1.21.0. Order: 1.21 < 1.21rc1 < 1.21.0 < 1.21.1.
type goVersion struct {
	major, minor int
	kind         int // 0 language version, 1 beta, 2 rc, 3 release
	n            int // beta/rc number or patch
}

func parseGoVersion(v string) (goVersion, bool) {
	var g goVersion
	major, rest, ok := strings.Cut(v, ".")
	if !ok {
		return g, false
	}
	mj, err := strconv.Atoi(major)
	if err != nil || mj < 1 || major != strconv.Itoa(mj) {
		return g, false
	}
	g.major = mj
	i := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		i++
	}
	if i == 0 {
		return g, false
	}
	mn, err := strconv.Atoi(rest[:i])
	if err != nil || rest[:i] != strconv.Itoa(mn) {
		return g, false
	}
	g.minor = mn
	rest = rest[i:]
	num := func(s string) (int, bool) {
		x, err := strconv.Atoi(s)
		return x, err == nil && x >= 0 && s == strconv.Itoa(x)
	}
	switch {
	case rest == "":
		g.kind = 0
	case strings.HasPrefix(rest, "rc"):
		g.kind = 2
		g.n, ok = num(rest[2:])
	case strings.HasPrefix(rest, "beta"):
		g.kind = 1
		g.n, ok = num(rest[4:])
	case strings.HasPrefix(rest, "."):
		g.kind = 3
		g.n, ok = num(rest[1:])
	default:
		return g, false
	}
	return g, ok
}

// sameRelease reports whether a and b are a language version and its first
// release, such as 1.21 and 1.21.0, in either order.
func sameRelease(a, b string) bool {
	x, okx := parseGoVersion(a)
	y, oky := parseGoVersion(b)
	if !okx || !oky || x.major != y.major || x.minor != y.minor {
		return false
	}
	return x.kind == 0 && y.kind == 3 && y.n == 0 || y.kind == 0 && x.kind == 3 && x.n == 0
}

func compareGo(a, b string) int {
	x, okx := parseGoVersion(a)
	y, oky := parseGoVersion(b)
	switch {
	case !okx && !oky:
		return strings.Compare(a, b)
	case !okx:
		return -1
	case !oky:
		return 1
	}
	for _, d := range [][2]int{{x.major, y.major}, {x.minor, y.minor}, {x.kind, y.kind}, {x.n, y.n}} {
		if d[0] != d[1] {
			if d[0] < d[1] {
				return -1
			}
			return 1
		}
	}
	return 0
}
