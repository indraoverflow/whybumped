// Package tests runs the whybumped command against the example modules,
// using the go command on the test machine and a module proxy made of files
// in examples/proxy, so no network is needed.
package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

var (
	buildOnce sync.Once
	toolPath  string
	buildErr  error
	modCache  string
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "whybumped-modcache-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	modCache = dir
	code := m.Run()
	os.RemoveAll(dir)
	if toolPath != "" {
		os.RemoveAll(filepath.Dir(toolPath))
	}
	os.Exit(code)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(wd)
}

func tool(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "whybumped-bin-")
		if err != nil {
			buildErr = err
			return
		}
		exe := "whybumped"
		if runtime.GOOS == "windows" {
			exe += ".exe"
		}
		toolPath = filepath.Join(dir, exe)
		cmd := exec.Command("go", "build", "-o", toolPath, "./plugin/scripts")
		cmd.Dir = repoRoot(t)
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("build: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return toolPath
}

// fixtureEnv points the go command at the file proxy in examples/proxy.
func fixtureEnv(t *testing.T) []string {
	t.Helper()
	proxy := filepath.ToSlash(filepath.Join(repoRoot(t), "examples", "proxy"))
	if !strings.HasPrefix(proxy, "/") {
		proxy = "/" + proxy
	}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "GOPROXY", "GOSUMDB", "GOTOOLCHAIN", "GOFLAGS", "GOMODCACHE", "GONOSUMDB", "GONOPROXY", "GOPRIVATE", "GOWORK", "GONOSUMCHECK", "GOINSECURE":
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"GOPROXY=file://"+proxy,
		"GOSUMDB=off",
		"GOTOOLCHAIN=local",
		"GOFLAGS=-modcacherw",
		"GOMODCACHE="+modCache,
	)
}

type result struct {
	code           int
	stdout, stderr string
}

func runEnv(t *testing.T, dir string, env []string, args ...string) result {
	t.Helper()
	cmd := exec.Command(tool(t), args...)
	cmd.Dir = dir
	cmd.Env = env
	var o, e bytes.Buffer
	cmd.Stdout = &o
	cmd.Stderr = &e
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return result{code, o.String(), e.String()}
}

func runTool(t *testing.T, args ...string) result {
	t.Helper()
	return runEnv(t, repoRoot(t), fixtureEnv(t), args...)
}

func example(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", name)
}

// copyExample copies an example module so a test can damage it.
func copyExample(t *testing.T, name string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"go.mod", "go.sum"} {
		b, err := os.ReadFile(filepath.Join(example(t, name), f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func mustContain(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
}

func TestExampleBump(t *testing.T) {
	r := runTool(t, "--old", example(t, "checkout-v1"), "--new", example(t, "checkout-v2"))
	if r.code != 1 {
		t.Fatalf("exit %d, want 1\n%s%s", r.code, r.stdout, r.stderr)
	}
	mustContain(t, r.stdout,
		"5 modules before, 6 after, 1 unchanged",
		"WARN  go-raised go 1.21 -> 1.22",
		"example.com/telemetry@v0.6.0 needs go 1.22",
		"WARN  added example.com/backoff v0.2.0",
		"via example.com/retry@v1.3.0 <- example.com/payclient@v1.5.0 <- go.mod",
		"WARN  raised example.com/retry v1.0.0 -> v1.3.0",
		"WARN  raised example.com/telemetry v0.4.0 -> v0.6.0",
		"INFO  raised example.com/payclient v1.4.0 -> v1.5.0",
		"INFO  raised example.com/textutil v1.0.0 -> v1.1.0",
		"dependencies need at most v1.0.0",
		"pulled in through: example.com/payclient@v1.5.0 (3)",
		"4 warnings, 2 info",
	)
	if r.stderr != "" {
		t.Errorf("stderr: %s", r.stderr)
	}
	r = runTool(t, "--old", example(t, "checkout-v1"), "--new", example(t, "checkout-v2"), "--fail-on", "never")
	if r.code != 0 || !strings.Contains(r.stdout, "go-raised") {
		t.Errorf("--fail-on never: exit %d\n%s", r.code, r.stdout)
	}
}

func TestExampleQuiet(t *testing.T) {
	r := runTool(t, "--old", example(t, "checkout-v1"), "--new", example(t, "checkout-v1-tidy"), "--fail-on", "warn")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0\n%s%s", r.code, r.stdout, r.stderr)
	}
	mustContain(t, r.stdout, "5 unchanged", "no dependency changes")
	for _, name := range []string{"checkout-v1", "checkout-v2", "checkout-v1-tidy"} {
		r := runTool(t, "--old", example(t, name), "--new", example(t, name))
		if r.code != 0 || !strings.Contains(r.stdout, "no dependency changes") {
			t.Errorf("%s against itself: exit %d\n%s%s", name, r.code, r.stdout, r.stderr)
		}
	}
}

func TestExampleReverse(t *testing.T) {
	// Going back from v2 to v1 lowers and removes modules.
	r := runTool(t, "--old", example(t, "checkout-v2"), "--new", example(t, "checkout-v1"))
	if r.code != 1 {
		t.Fatalf("exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	mustContain(t, r.stdout,
		"INFO  go-lowered go 1.22 -> 1.21",
		"WARN  lowered example.com/retry v1.3.0 -> v1.0.0",
		"example.com/payclient@v1.5.0 required v1.3.0 before",
		"INFO  removed example.com/backoff v0.2.0",
		"WARN  lowered example.com/payclient v1.5.0 -> v1.4.0",
	)
}

func TestExpect(t *testing.T) {
	old, nw := example(t, "checkout-v1"), example(t, "checkout-v2")
	r := runTool(t, "--old", old, "--new", nw, "--expect", "example.com/payclient@v1.5.0")
	if r.code != 1 {
		t.Fatalf("exit %d\n%s", r.code, r.stdout)
	}
	mustContain(t, r.stdout,
		"INFO  raised example.com/payclient v1.4.0 -> v1.5.0\n        expected;",
		"WARN  raised example.com/textutil v1.0.0 -> v1.1.0\n        not in --expect;",
	)
	r = runTool(t, "--old", old, "--new", nw, "--expect", "example.com/payclient@v1.6.0")
	mustContain(t, r.stdout, "WARN  raised example.com/payclient v1.4.0 -> v1.5.0\n        expected v1.6.0 but got v1.5.0")
	r = runTool(t, "--old", old, "--new", nw, "--expect", "example.com/httpkit")
	mustContain(t, r.stdout, "WARN  expect-missed example.com/httpkit v1.2.0", "the selected version didn't change (v1.2.0)")
	r = runTool(t, "--old", old, "--new", example(t, "checkout-v1-tidy"), "--expect", "example.com/payclient")
	if r.code != 1 || !strings.Contains(r.stdout, "expect-missed example.com/payclient") {
		t.Errorf("an expected change that didn't happen must fail: exit %d\n%s", r.code, r.stdout)
	}
}

func TestJSON(t *testing.T) {
	r := runTool(t, "--old", example(t, "checkout-v1"), "--new", example(t, "checkout-v2"), "--format", "json")
	if r.code != 1 {
		t.Fatalf("exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	var rep struct {
		Module   string
		Findings []struct {
			Severity, Change, Module, Old, New, Cause string
			Chain                                     []string
		}
		Roots []struct {
			Node    string
			Changes int
		}
	}
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Module != "example.com/checkout" || len(rep.Findings) != 6 {
		t.Fatalf("report: %+v", rep)
	}
	if rep.Findings[0].Change != "go-raised" || rep.Findings[0].Chain[0] != "example.com/telemetry@v0.6.0" {
		t.Errorf("first finding: %+v", rep.Findings[0])
	}
	if len(rep.Roots) != 1 || rep.Roots[0].Node != "example.com/payclient@v1.5.0" || rep.Roots[0].Changes != 3 {
		t.Errorf("roots: %+v", rep.Roots)
	}
	r = runTool(t, "--old", example(t, "checkout-v1"), "--new", example(t, "checkout-v1-tidy"), "--format", "json")
	if r.code != 0 || !strings.Contains(r.stdout, `"findings": []`) {
		t.Errorf("quiet JSON: exit %d\n%s", r.code, r.stdout)
	}
}

const buildLog = `# example.com/checkout
./main.go:12:2: undefined: payclient.NewClient
./main.go:15:9: cannot use x (variable of type int) as string value in return statement
`

const testLog = `--- FAIL: TestCharge (0.00s)
    charge_test.go:21: got 500, want 200
FAIL
FAIL	example.com/checkout	0.012s
FAIL
`

const goErrLog = `go: example.com/payclient@v1.5.0: reading https://proxy.golang.org/example.com/payclient/@v/v1.5.0.mod: 404 Not Found
	server response: not found
`

// damage changes a copied module so it can't be trusted.
type damage struct {
	name  string
	apply func(t *testing.T, dir string) string // returns the path to pass
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func goModDamage(name string, f func(orig string) string) damage {
	return damage{name, func(t *testing.T, dir string) string {
		p := filepath.Join(dir, "go.mod")
		writeFile(t, p, f(readFile(t, p)))
		return dir
	}}
}

func goSumDamage(name string, f func(orig string) string) damage {
	return damage{name, func(t *testing.T, dir string) string {
		p := filepath.Join(dir, "go.sum")
		writeFile(t, p, f(readFile(t, p)))
		return dir
	}}
}

func cutAfter(s, marker string) string {
	i := strings.Index(s, marker)
	if i < 0 {
		panic("marker not found: " + marker)
	}
	return s[:i+len(marker)]
}

func damages() []damage {
	return []damage{
		{"missing dir", func(t *testing.T, dir string) string { return filepath.Join(dir, "nope") }},
		{"file instead of dir", func(t *testing.T, dir string) string { return filepath.Join(dir, "go.mod") }},
		{"empty dir", func(t *testing.T, dir string) string { d := t.TempDir(); return d }},
		{"no go.mod", func(t *testing.T, dir string) string { os.Remove(filepath.Join(dir, "go.mod")); return dir }},
		goModDamage("empty go.mod", func(string) string { return "" }),
		goModDamage("binary go.mod", func(string) string { return "\x7fELF\x02\x01\x01\x00\x00\x00\n" }),
		goModDamage("garbage go.mod", func(string) string { return "lorem ipsum dolor\nsit amet\n" }),
		goModDamage("go.mod cut mid line", func(s string) string { return s[:len(s)/2] }),
		goModDamage("go.mod cut inside require block", func(s string) string { return cutAfter(s, "example.com/httpkit v1.2.0\n") }),
		goModDamage("go.mod cut after go line", func(s string) string { return s[:strings.Index(s, "\nrequire")] }),
		goModDamage("go.mod cut before go line", func(s string) string { return cutAfter(s, "example.com/checkout\n") }),
		goModDamage("go.mod is build output", func(string) string { return buildLog }),
		goModDamage("go.mod is test output", func(string) string { return testLog }),
		goModDamage("go.mod is go error output", func(string) string { return goErrLog }),
		goModDamage("other module", func(s string) string {
			return strings.Replace(s, "module example.com/checkout", "module example.com/billing", 1)
		}),
		goModDamage("unknown version", func(s string) string {
			return strings.Replace(s, "example.com/httpkit v1.2.0", "example.com/httpkit v1.9.0", 1)
		}),
		goModDamage("unknown module", func(s string) string {
			return strings.Replace(s, "require (", "require (\n\texample.com/nowhere v1.0.0", 1)
		}),
		{"no go.sum", func(t *testing.T, dir string) string { os.Remove(filepath.Join(dir, "go.sum")); return dir }},
		goSumDamage("empty go.sum", func(string) string { return "" }),
		goSumDamage("garbage go.sum", func(string) string { return "lorem ipsum\n" }),
		goSumDamage("go.sum cut mid line", func(s string) string { return s[:len(s)/2] }),
		goSumDamage("go.sum cut at a line", func(s string) string { return s[:strings.Index(s, "\n")+1] }),
		goSumDamage("go.sum is build output", func(string) string { return buildLog }),
		goSumDamage("go.sum wrong hash", func(s string) string {
			return strings.Replace(s, "example.com/httpkit v1.2.0/go.mod h1:6", "example.com/httpkit v1.2.0/go.mod h1:7", 1)
		}),
	}
}

func TestDamagedSides(t *testing.T) {
	flagSets := [][]string{nil, {"--fail-on", "never"}, {"--fail-on", "warn"}, {"--format", "json"}, {"--fail-on", "never", "--format", "json"}}
	for _, d := range damages() {
		for _, which := range []string{"old", "new"} {
			for _, pair := range [][2]string{{"checkout-v1", "checkout-v2"}, {"checkout-v1", "checkout-v1"}} {
				t.Run(fmt.Sprintf("%s/%s/%s-%s", d.name, which, pair[0], pair[1]), func(t *testing.T) {
					old, nw := example(t, pair[0]), example(t, pair[1])
					if which == "old" {
						old = d.apply(t, copyExample(t, pair[0]))
					} else {
						nw = d.apply(t, copyExample(t, pair[1]))
					}
					for _, flags := range flagSets {
						args := append([]string{"--old", old, "--new", nw}, flags...)
						r := runTool(t, args...)
						if r.code != 2 || r.stdout != "" {
							t.Errorf("%v: exit %d, want 2 with empty stdout\nstdout: %s\nstderr: %s", flags, r.code, r.stdout, r.stderr)
						}
						if !strings.Contains(r.stderr, "whybumped:") {
							t.Errorf("%v: no error message: %q", flags, r.stderr)
						}
					}
				})
			}
		}
	}
}

func TestDamagedInputLeavesFilesAlone(t *testing.T) {
	dir := copyExample(t, "checkout-v2")
	writeFile(t, filepath.Join(dir, "go.sum"), "")
	env := append(fixtureEnv(t), "GOFLAGS=-modcacherw -mod=mod")
	r := runEnv(t, repoRoot(t), env, "--old", example(t, "checkout-v1"), "--new", dir)
	if r.code != 2 {
		t.Errorf("exit %d, want 2\n%s%s", r.code, r.stdout, r.stderr)
	}
	if got := readFile(t, filepath.Join(dir, "go.sum")); got != "" {
		t.Errorf("whybumped changed go.sum:\n%s", got)
	}
}

// fakeGo writes a go command that prints the given output for one
// directory and runs the real go command everywhere else.
func fakeGo(t *testing.T, target, mode string) []string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake go command is a shell script")
	}
	realGo, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
if [ "$(pwd -P)" != %q ]; then exec %q "$@"; fi
data=%q
case "$1 $2" in
"mod graph") kind=graph ;;
"list -m") kind=list ;;
*) exec %q "$@" ;;
esac
. "$data/mode.sh"
`, mustEval(t, target), realGo, bin, realGo)
	writeFile(t, filepath.Join(bin, "go"), script)
	if err := os.Chmod(filepath.Join(bin, "go"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Real outputs of the target, to damage.
	env := fixtureEnv(t)
	graph := goOut(t, target, env, "mod", "graph")
	list := goOut(t, target, env, "list", "-m", "-json", "all")
	writeFile(t, filepath.Join(bin, "graph.txt"), graph)
	writeFile(t, filepath.Join(bin, "list.json"), list)
	writeFile(t, filepath.Join(bin, "mode.sh"), mode)
	var out []string
	for _, kv := range env {
		if !strings.HasPrefix(kv, "PATH=") {
			out = append(out, kv)
		}
	}
	return append(out, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func goOut(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(env, "GOWORK=off")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go %v: %v", args, err)
	}
	return string(out)
}

func TestFakeGoOutputs(t *testing.T) {
	modes := map[string]string{
		"graph empty":                `if [ $kind = graph ]; then exit 0; fi; cat "$data/list.json"`,
		"graph garbage":              `if [ $kind = graph ]; then echo "hello world how are you"; exit 0; fi; cat "$data/list.json"`,
		"graph cut mid line":         `if [ $kind = graph ]; then head -c 150 "$data/graph.txt"; exit 0; fi; cat "$data/list.json"`,
		"graph cut at a line":        `if [ $kind = graph ]; then head -n 3 "$data/graph.txt"; exit 0; fi; cat "$data/list.json"`,
		"graph without main edges":   `if [ $kind = graph ]; then grep -v '^example.com/checkout ' "$data/graph.txt"; exit 0; fi; cat "$data/list.json"`,
		"graph partial then failure": `if [ $kind = graph ]; then head -n 4 "$data/graph.txt"; echo "go: missing go.sum entry" >&2; exit 1; fi; cat "$data/list.json"`,
		"graph is build output":      `if [ $kind = graph ]; then printf '%s\n' '# example.com/checkout' './main.go:1:1: expected package'; exit 0; fi; cat "$data/list.json"`,
		"list empty":                 `if [ $kind = list ]; then exit 0; fi; cat "$data/graph.txt"`,
		"list garbage":               `if [ $kind = list ]; then echo "not json"; exit 0; fi; cat "$data/graph.txt"`,
		"list cut off":               `if [ $kind = list ]; then head -c 300 "$data/list.json"; exit 0; fi; cat "$data/graph.txt"`,
		"list failure":               `if [ $kind = list ]; then cat "$data/list.json"; echo "go: updates to go.mod needed" >&2; exit 1; fi; cat "$data/graph.txt"`,
		"list with error":            `if [ $kind = list ]; then cat "$data/list.json"; echo '{"Path":"example.com/x","Version":"v1.0.0","Error":{"Err":"not found"}}'; exit 0; fi; cat "$data/graph.txt"`,
		"list missing a module":      `if [ $kind = list ]; then sed -n '1,/^}/p' "$data/list.json"; exit 0; fi; cat "$data/graph.txt"`,
		"list without main":          `if [ $kind = list ]; then sed '1,/^}/d' "$data/list.json"; exit 0; fi; cat "$data/graph.txt"`,
		"list other main":            `if [ $kind = list ]; then sed 's#"Path": "example.com/checkout"#"Path": "example.com/other"#' "$data/list.json"; exit 0; fi; cat "$data/graph.txt"`,
		"list wrong version":         `if [ $kind = list ]; then awk '!done && sub(/"Version": "v/, "\"Version\": \"v9") { done = 1 } { print }' "$data/list.json"; exit 0; fi; cat "$data/graph.txt"`,
	}
	for name, mode := range modes {
		for _, which := range []string{"old", "new"} {
			t.Run(name+"/"+which, func(t *testing.T) {
				old, nw := example(t, "checkout-v1"), example(t, "checkout-v2")
				target := nw
				if which == "old" {
					target = old
				}
				env := fakeGo(t, target, mode)
				for _, flags := range [][]string{nil, {"--fail-on", "never"}, {"--format", "json"}} {
					r := runEnv(t, repoRoot(t), env, append([]string{"--old", old, "--new", nw}, flags...)...)
					if r.code != 2 || r.stdout != "" {
						t.Errorf("%v: exit %d, want 2 with empty stdout\nstdout: %s\nstderr: %s", flags, r.code, r.stdout, r.stderr)
					}
				}
			})
		}
	}
	// The fake command with real outputs must give the normal result.
	t.Run("passthrough", func(t *testing.T) {
		old, nw := example(t, "checkout-v1"), example(t, "checkout-v2")
		env := fakeGo(t, nw, `if [ $kind = graph ]; then cat "$data/graph.txt"; else cat "$data/list.json"; fi`)
		r := runEnv(t, repoRoot(t), env, "--old", old, "--new", nw)
		if r.code != 1 || !strings.Contains(r.stdout, "go-raised") {
			t.Errorf("exit %d\n%s%s", r.code, r.stdout, r.stderr)
		}
	})
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestBase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(repo, "README"), "hi\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "empty")
	git(t, repo, "tag", "nomodule")
	svc := filepath.Join(repo, "svc")
	os.MkdirAll(svc, 0o755)
	for _, f := range []string{"go.mod", "go.sum"} {
		writeFile(t, filepath.Join(svc, f), readFile(t, filepath.Join(example(t, "checkout-v1"), f)))
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "v1")
	git(t, repo, "tag", "v1")
	for _, f := range []string{"go.mod", "go.sum"} {
		writeFile(t, filepath.Join(svc, f), readFile(t, filepath.Join(example(t, "checkout-v2"), f)))
	}

	r := runTool(t, "--base", "v1", "--new", svc)
	if r.code != 1 {
		t.Fatalf("exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	mustContain(t, r.stdout, "whybumped v1 -> "+svc, "go-raised go 1.21 -> 1.22", "pulled in through: example.com/payclient@v1.5.0 (3)")

	git(t, repo, "commit", "-q", "-am", "v2")
	r = runTool(t, "--base", "HEAD", "--new", svc)
	if r.code != 0 || !strings.Contains(r.stdout, "no dependency changes") {
		t.Errorf("HEAD against an unchanged tree: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	r = runTool(t, "--base", "HEAD~1", "--new", svc, "--fail-on", "never")
	if r.code != 0 || !strings.Contains(r.stdout, "go-raised") {
		t.Errorf("HEAD~1: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}

	for _, ref := range []string{"nosuchref", "nomodule", "--output=/tmp/x", "-h", "v1:svc/go.mod"} {
		for _, flags := range [][]string{nil, {"--fail-on", "never"}} {
			r := runTool(t, append([]string{"--base", ref, "--new", svc}, flags...)...)
			if r.code != 2 || r.stdout != "" {
				t.Errorf("--base %s %v: exit %d\n%s%s", ref, flags, r.code, r.stdout, r.stderr)
			}
		}
	}
	outside := copyExample(t, "checkout-v2")
	r = runTool(t, "--base", "HEAD", "--new", outside)
	if r.code != 2 || r.stdout != "" {
		t.Errorf("outside git: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
}

func TestBaseLocalReplace(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	svc, lib := filepath.Join(repo, "svc"), filepath.Join(repo, "lib")
	os.MkdirAll(svc, 0o755)
	os.MkdirAll(lib, 0o755)
	writeFile(t, filepath.Join(svc, "go.mod"), readFile(t, filepath.Join(example(t, "checkout-v1"), "go.mod"))+
		"\nrequire example.com/lib v0.0.0\n\nreplace example.com/lib => ../lib\n")
	writeFile(t, filepath.Join(svc, "go.sum"), readFile(t, filepath.Join(example(t, "checkout-v1"), "go.sum")))
	writeFile(t, filepath.Join(lib, "go.mod"), "module example.com/lib\n\ngo 1.21\n\nrequire example.com/retry v1.0.0\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "v1")

	// The sibling module now needs a newer retry. The service's go.mod and
	// go.sum are updated the way the go command would.
	writeFile(t, filepath.Join(lib, "go.mod"), "module example.com/lib\n\ngo 1.21\n\nrequire example.com/retry v1.3.0\n")
	mod := readFile(t, filepath.Join(svc, "go.mod"))
	writeFile(t, filepath.Join(svc, "go.mod"), strings.Replace(mod, "example.com/retry v1.0.0", "example.com/retry v1.3.0", 1))
	writeFile(t, filepath.Join(svc, "go.sum"), readFile(t, filepath.Join(example(t, "checkout-v2"), "go.sum")))
	r := runTool(t, "--base", "HEAD", "--new", svc)
	if r.code != 1 {
		t.Fatalf("exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	mustContain(t, r.stdout,
		"WARN  raised example.com/retry v1.0.0 -> v1.3.0\n        raised to v1.3.0 because example.com/lib@v0.0.0 => ../lib requires it",
		"pulled in through: example.com/lib@v0.0.0 => ../lib (2)",
	)
}

func TestGoLineRespelled(t *testing.T) {
	// Newer go commands rewrite "go 1.21" as "go 1.21.0". That's the same
	// release and must not be reported as a raise.
	dir := copyExample(t, "checkout-v1")
	mod := readFile(t, filepath.Join(dir, "go.mod"))
	writeFile(t, filepath.Join(dir, "go.mod"), strings.Replace(mod, "\ngo 1.21\n", "\ngo 1.21.0\n", 1))
	r := runTool(t, "--old", example(t, "checkout-v1"), "--new", dir)
	if r.code != 0 {
		t.Fatalf("exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	mustContain(t, r.stdout, "INFO  go-respelled go 1.21 -> 1.21.0", "0 warnings, 1 info")
	// A real raise from 1.21.0 is still a raise, and the dependency at go
	// 1.21 doesn't count as needing more.
	r = runTool(t, "--old", dir, "--new", example(t, "checkout-v2"))
	mustContain(t, r.stdout, "WARN  go-raised go 1.21.0 -> 1.22", "example.com/telemetry@v0.6.0 needs go 1.22\n")
}

func TestGoModLostTrailingRequire(t *testing.T) {
	// A go.mod cut exactly at a line break can still parse. Losing a trailing
	// require must not look like a clean result.
	old := copyExample(t, "checkout-v2")
	writeFile(t, filepath.Join(old, "go.mod"), readFile(t, filepath.Join(old, "go.mod"))+"\nrequire example.com/backoff v0.2.0\n")
	writeFile(t, filepath.Join(old, "go.sum"), readFile(t, filepath.Join(old, "go.sum"))+
		"example.com/backoff v0.2.0/go.mod h1:m2yJIumG1FSf7ythazihIpahWMur6pZ/EiU3KNL1b8g=\n")
	for _, flags := range [][]string{nil, {"--fail-on", "warn"}} {
		r := runTool(t, append([]string{"--old", old, "--new", example(t, "checkout-v2")}, flags...)...)
		if r.code != 1 {
			t.Errorf("%v: exit %d, want 1\n%s%s", flags, r.code, r.stdout, r.stderr)
		}
		mustContain(t, r.stdout, "WARN  requirement-dropped example.com/backoff v0.2.0\n",
			"it is still selected at v0.2.0 because example.com/retry@v1.3.0 requires it")
	}
	// The other way round, the same line added is INFO and exits 0.
	r := runTool(t, "--old", example(t, "checkout-v2"), "--new", old)
	if r.code != 0 {
		t.Errorf("exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
}
