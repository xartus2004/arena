// Package env loads environment and task specifications from YAML.
//
// An environment directory looks like:
//
//	environments/fix-the-bug/
//	  env.yaml            # sandbox limits + list of tasks
//	  tasks/<id>/files/   # student-visible files (copied into the sandbox)
//	  tasks/<id>/hidden/  # hidden files (grader sandbox only)
//
// Hidden files are only ever read by the platform and copied into the
// grader sandbox; the env package makes this explicit by exposing them via a
// separate accessor (HiddenTar) that the student path never calls.
package env

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	rt "github.com/xartus2004/arena/runtime"
)

// Size is a byte count that unmarshals from strings like "64Mi", "1G", "512k".
type Size int64

// UnmarshalYAML implements yaml.Unmarshaler.
func (s *Size) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseSize(n.Value)
	if err != nil {
		return err
	}
	*s = Size(v)
	return nil
}

// ParseSize parses a byte size with an optional binary or decimal suffix.
func ParseSize(str string) (int64, error) {
	str = strings.TrimSpace(str)
	if str == "" {
		return 0, nil
	}
	mult := map[string]int64{
		"": 1, "b": 1,
		"k": 1e3, "kb": 1e3, "ki": 1 << 10, "kib": 1 << 10,
		"m": 1e6, "mb": 1e6, "mi": 1 << 20, "mib": 1 << 20,
		"g": 1e9, "gb": 1e9, "gi": 1 << 30, "gib": 1 << 30,
	}
	i := strings.IndexFunc(str, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	num, suf := str, ""
	if i >= 0 {
		num, suf = str[:i], strings.ToLower(strings.TrimSpace(str[i:]))
	}
	m, ok := mult[suf]
	if !ok {
		return 0, fmt.Errorf("size %q: unknown suffix %q", str, suf)
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("size %q: invalid number", str)
	}
	return int64(f * float64(m)), nil
}

// Duration unmarshals from Go duration strings ("90s", "5m").
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("duration %q: %w", n.Value, err)
	}
	*d = Duration(v)
	return nil
}

// Sandbox is the YAML form of a runtime.Spec.
type Sandbox struct {
	Image    string            `yaml:"image"`
	CPUs     float64           `yaml:"cpus"`
	Memory   Size              `yaml:"memory"`
	PIDs     int64             `yaml:"pids"`
	Disk     Size              `yaml:"disk"`
	Inodes   int64             `yaml:"inodes"`
	Network  rt.NetworkMode    `yaml:"network"`
	Allow    []string          `yaml:"allow"`
	Lifetime Duration          `yaml:"lifetime"`
	Workdirs []string          `yaml:"workdirs"`
	Labels   map[string]string `yaml:"labels"`
}

// Spec converts to a runtime.Spec.
func (s Sandbox) Spec() rt.Spec {
	return rt.Spec{
		Image:       s.Image,
		CPUs:        s.CPUs,
		MemoryBytes: int64(s.Memory),
		MaxPIDs:     s.PIDs,
		DiskBytes:   int64(s.Disk),
		MaxInodes:   s.Inodes,
		Network:     rt.NetworkPolicy{Mode: s.Network, Allow: s.Allow},
		Timeout:     time.Duration(s.Lifetime),
		Workdirs:    s.Workdirs,
		Labels:      s.Labels,
	}
}

// Step is a setup command.
type Step struct {
	Run     string   `yaml:"run"`
	User    string   `yaml:"user"` // root (default) | student | service
	Timeout Duration `yaml:"timeout"`
}

// Grader describes how a submission is scored.
type Grader struct {
	// Command runs as root in the grader sandbox with the submission at
	// /submission and hidden files at /grader. It must print a JSON object
	// {"reward": float, "metadata": {...}} as the last line of stdout.
	Command []string `yaml:"command"`
	Timeout Duration `yaml:"timeout"`
	// Setup steps run in the grader sandbox before the command.
	Setup []Step `yaml:"setup"`
}

// Limits bounds what custody accepts as a submission.
type Limits struct {
	MaxFiles     int  `yaml:"max_files"`
	MaxBytes     Size `yaml:"max_bytes"`
	MaxFileBytes Size `yaml:"max_file_bytes"`
	MaxDepth     int  `yaml:"max_depth"`
}

// Task is one problem within an environment.
type Task struct {
	ID           string `yaml:"id"`
	Instructions string `yaml:"instructions"`
	Files        string `yaml:"files"` // dir relative to env dir; copied to FilesDest
	FilesDest    string `yaml:"files_dest"`
	Hidden       string `yaml:"hidden"` // dir relative to env dir; grader only
	Setup        []Step `yaml:"setup"`
	Submission   string `yaml:"submission"`
	// KillUsers lists extra uids (besides the student) to stop before
	// custody, e.g. an in-sandbox service whose DB is the submission.
	KillUsers []string `yaml:"kill_users"`
	Grader    Grader   `yaml:"grader"`
	TimeLimit Duration `yaml:"time_limit"`
	MaxSteps  int      `yaml:"max_steps"`
	Limits    *Limits  `yaml:"limits"`

	env *Environment
}

// Env returns the environment the task belongs to.
func (t *Task) Env() *Environment { return t.env }

// Environment is a loaded env.yaml.
type Environment struct {
	Name          string   `yaml:"name"`
	Description   string   `yaml:"description"`
	Sandbox       Sandbox  `yaml:"sandbox"`
	GraderSandbox *Sandbox `yaml:"grader_sandbox"`
	Limits        Limits   `yaml:"limits"`
	Tasks         []*Task  `yaml:"tasks"`

	dir string
}

// Dir returns the directory the environment was loaded from.
func (e *Environment) Dir() string { return e.dir }

// Task returns the task with the given id.
func (e *Environment) Task(id string) (*Task, error) {
	for _, t := range e.Tasks {
		if t.ID == id {
			return t, nil
		}
	}
	return nil, fmt.Errorf("env %s: no task %q", e.Name, id)
}

// StudentSpec is the runtime spec of the student sandbox.
func (e *Environment) StudentSpec() rt.Spec { return e.Sandbox.Spec() }

// GraderSpec is the runtime spec of the grader sandbox.
func (e *Environment) GraderSpec() rt.Spec {
	if e.GraderSandbox != nil {
		return e.GraderSandbox.Spec()
	}
	s := e.Sandbox.Spec()
	s.Workdirs = []string{"/grader", "/submission"}
	return s
}

// EffectiveLimits returns task limits falling back to environment limits and
// then to safe defaults.
func (t *Task) EffectiveLimits() Limits {
	l := t.env.Limits
	if t.Limits != nil {
		o := *t.Limits
		if o.MaxFiles > 0 {
			l.MaxFiles = o.MaxFiles
		}
		if o.MaxBytes > 0 {
			l.MaxBytes = o.MaxBytes
		}
		if o.MaxFileBytes > 0 {
			l.MaxFileBytes = o.MaxFileBytes
		}
		if o.MaxDepth > 0 {
			l.MaxDepth = o.MaxDepth
		}
	}
	if l.MaxFiles <= 0 {
		l.MaxFiles = 1000
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = 16 << 20
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = l.MaxBytes
	}
	if l.MaxDepth <= 0 {
		l.MaxDepth = 16
	}
	return l
}

// UserByName maps a step user name to a runtime.User.
func UserByName(name string) (rt.User, error) {
	switch name {
	case "", "root":
		return rt.Root, nil
	case "student":
		return rt.Student, nil
	case "service":
		return rt.Service, nil
	case "untrusted":
		return rt.Untrusted, nil
	}
	return rt.User{}, fmt.Errorf("unknown user %q", name)
}

// Load reads dir/env.yaml and validates it.
func Load(dir string) (*Environment, error) {
	b, err := os.ReadFile(filepath.Join(dir, "env.yaml"))
	if err != nil {
		return nil, fmt.Errorf("load env %s: %w", dir, err)
	}
	e, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("load env %s: %w", dir, err)
	}
	e.dir = dir
	for _, t := range e.Tasks {
		for _, d := range []string{t.Files, t.Hidden} {
			if d == "" {
				continue
			}
			if st, err := os.Stat(filepath.Join(dir, d)); err != nil || !st.IsDir() {
				return nil, fmt.Errorf("load env %s: task %s: %s is not a directory", dir, t.ID, d)
			}
		}
	}
	return e, nil
}

// Parse parses and validates env YAML (paths are not checked).
func Parse(b []byte) (*Environment, error) {
	var e Environment
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&e); err != nil {
		return nil, fmt.Errorf("parse env yaml: %w", err)
	}
	e.applyDefaults()
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return &e, nil
}

func (e *Environment) applyDefaults() {
	sb := &e.Sandbox
	if sb.Network == "" {
		sb.Network = rt.NetworkNone
	}
	if sb.Lifetime == 0 {
		sb.Lifetime = Duration(30 * time.Minute)
	}
	if len(sb.Workdirs) == 0 {
		sb.Workdirs = []string{"/workspace"}
	}
	for _, t := range e.Tasks {
		t.env = e
		if t.TimeLimit == 0 {
			t.TimeLimit = Duration(10 * time.Minute)
		}
		if t.MaxSteps == 0 {
			t.MaxSteps = 100
		}
		if t.FilesDest == "" {
			t.FilesDest = "/workspace"
		}
		if t.Grader.Timeout == 0 {
			t.Grader.Timeout = Duration(2 * time.Minute)
		}
	}
}

// Validate checks the environment for consistency.
func (e *Environment) Validate() error {
	var errs []error
	if e.Name == "" {
		errs = append(errs, errors.New("name is required"))
	}
	if err := e.Sandbox.Spec().Validate(); err != nil {
		errs = append(errs, fmt.Errorf("sandbox: %w", err))
	}
	if e.GraderSandbox != nil {
		if err := e.GraderSandbox.Spec().Validate(); err != nil {
			errs = append(errs, fmt.Errorf("grader_sandbox: %w", err))
		}
	}
	if len(e.Tasks) == 0 {
		errs = append(errs, errors.New("at least one task is required"))
	}
	seen := map[string]bool{}
	for i, t := range e.Tasks {
		where := fmt.Sprintf("task[%d] %q", i, t.ID)
		if t.ID == "" {
			errs = append(errs, fmt.Errorf("%s: id is required", where))
		}
		if seen[t.ID] {
			errs = append(errs, fmt.Errorf("%s: duplicate id", where))
		}
		seen[t.ID] = true
		if strings.TrimSpace(t.Instructions) == "" {
			errs = append(errs, fmt.Errorf("%s: instructions are required", where))
		}
		if !path.IsAbs(t.Submission) || path.Clean(t.Submission) == "/" {
			errs = append(errs, fmt.Errorf("%s: submission must be an absolute path below /", where))
		}
		if len(t.Grader.Command) == 0 {
			errs = append(errs, fmt.Errorf("%s: grader.command is required", where))
		}
		if t.Files != "" && t.Hidden != "" && path.Clean(t.Files) == path.Clean(t.Hidden) {
			errs = append(errs, fmt.Errorf("%s: files and hidden must differ", where))
		}
		for _, s := range append(append([]Step{}, t.Setup...), t.Grader.Setup...) {
			if _, err := UserByName(s.User); err != nil {
				errs = append(errs, fmt.Errorf("%s: setup: %w", where, err))
			}
		}
		for _, u := range t.KillUsers {
			if _, err := UserByName(u); err != nil {
				errs = append(errs, fmt.Errorf("%s: kill_users: %w", where, err))
			}
		}
	}
	return errors.Join(errs...)
}

// FilesTar returns a tar stream of the task's student-visible files.
func (t *Task) FilesTar() (*bytes.Buffer, error) {
	return tarDir(t.env.dir, t.Files)
}

// HiddenTar returns a tar stream of the task's hidden files. Only the grader
// path may call this.
func (t *Task) HiddenTar() (*bytes.Buffer, error) {
	return tarDir(t.env.dir, t.Hidden)
}

// tarDir tars a directory tree. Only regular files and directories are
// allowed (environment authors can't accidentally ship symlinks).
func tarDir(base, rel string) (*bytes.Buffer, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if rel == "" {
		return &buf, tw.Close()
	}
	root := filepath.Join(base, rel)
	var paths []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != root {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("tar %s: %w", root, err)
	}
	sort.Strings(paths)
	for _, p := range paths {
		info, err := os.Lstat(p)
		if err != nil {
			return nil, fmt.Errorf("tar %s: %w", p, err)
		}
		name, _ := filepath.Rel(root, p)
		name = filepath.ToSlash(name)
		h := &tar.Header{Name: name, Mode: int64(info.Mode().Perm()), ModTime: info.ModTime()}
		switch {
		case info.IsDir():
			h.Typeflag, h.Name = tar.TypeDir, name+"/"
		case info.Mode().IsRegular():
			h.Typeflag, h.Size = tar.TypeReg, info.Size()
		default:
			return nil, fmt.Errorf("tar %s: only regular files and directories are allowed", p)
		}
		if err := tw.WriteHeader(h); err != nil {
			return nil, fmt.Errorf("tar %s: %w", p, err)
		}
		if h.Typeflag == tar.TypeReg {
			data, err := os.ReadFile(p)
			if err != nil {
				return nil, fmt.Errorf("tar %s: %w", p, err)
			}
			if _, err := tw.Write(data); err != nil {
				return nil, fmt.Errorf("tar %s: %w", p, err)
			}
		}
	}
	return &buf, tw.Close()
}

// LoadAll loads every environment under root (one per subdirectory that
// contains env.yaml).
func LoadAll(root string) (map[string]*Environment, error) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("load environments: %w", err)
	}
	out := map[string]*Environment{}
	for _, d := range ents {
		if !d.IsDir() {
			continue
		}
		dir := filepath.Join(root, d.Name())
		if _, err := os.Stat(filepath.Join(dir, "env.yaml")); err != nil {
			continue
		}
		e, err := Load(dir)
		if err != nil {
			return nil, err
		}
		out[e.Name] = e
	}
	return out, nil
}
