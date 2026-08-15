package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Receipt filename layout mirrors the manifest's.
const (
	ReceiptPrefix = "demonolith-migrate-"
	VerdictPrefix = "demonolith-verify-"
)

// MoveOutcome is what happened to one state move.
type MoveOutcome struct {
	Address string `yaml:"address"`
	Module  string `yaml:"module"`
	// Outcome is "moved" or "skipped" (already applied).
	Outcome string `yaml:"outcome"`
}

// Receipt is the execution record of one migrate run: the manifest is the
// plan, the receipt is what actually happened and where the artifacts are.
type Receipt struct {
	Version int    `yaml:"version"`
	Created string `yaml:"created"`
	Tool    string `yaml:"tool"`
	// Manifest is the filename (not path) of the manifest that was executed.
	Manifest string `yaml:"manifest"`
	Engine   string `yaml:"engine,omitempty"`
	// Complete is true when every manifest move is accounted for.
	Complete bool `yaml:"complete"`
	// ModuleStates maps a module to its carved state file, relative to root dir.
	ModuleStates map[string]string `yaml:"module_states"`
	// BackupPath is the pre-carve snapshot of the source state, relative to root dir.
	BackupPath string        `yaml:"backup_path"`
	Moves      []MoveOutcome `yaml:"moves"`
}

// ReceiptFileName renders the receipt filename for a timestamp.
func ReceiptFileName(t time.Time) string {
	return ReceiptPrefix + t.UTC().Format(TimeLayout) + ".yaml"
}

// WriteReceipt marshals the receipt into rootDir and returns its path.
func WriteReceipt(r *Receipt, rootDir string, created time.Time) (string, error) {
	b, err := yaml.Marshal(r)
	if err != nil {
		return "", err
	}
	path := filepath.Join(rootDir, ReceiptFileName(created))
	return path, os.WriteFile(path, b, 0o644)
}

// LoadReceipt reads one receipt.
func LoadReceipt(path string) (*Receipt, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Receipt
	if err := yaml.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("parse receipt %s: %w", path, err)
	}
	if r.Version > SchemaVersion {
		return nil, fmt.Errorf("receipt %s has schema version %d; this build supports up to %d", path, r.Version, SchemaVersion)
	}
	return &r, nil
}

// LatestReceiptFor finds the newest receipt in rootDir recording an execution
// of the named manifest file, or nil when none exists.
func LatestReceiptFor(rootDir, manifestFile string) (*Receipt, error) {
	entries, err := os.ReadDir(rootDir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() && strings.HasPrefix(name, ReceiptPrefix) && strings.HasSuffix(name, ".yaml") {
			paths = append(paths, filepath.Join(rootDir, name))
		}
	}
	sort.Strings(paths)
	for i := len(paths) - 1; i >= 0; i-- {
		r, err := LoadReceipt(paths[i])
		if err != nil {
			return nil, err
		}
		if r.Manifest == manifestFile {
			return r, nil
		}
	}
	return nil, nil
}

// ModuleVerdict is one module's proof result.
type ModuleVerdict struct {
	Module   string `yaml:"module" json:"module"`
	ZeroDiff bool   `yaml:"zero_diff" json:"zero_diff"`
	Create   int    `yaml:"create" json:"create"`
	Destroy  int    `yaml:"destroy" json:"destroy"`
	// Update counts in-place updates; they are reported but never fail the proof.
	Update int `yaml:"update" json:"update"`
}

// Verdict is the verify sidecar: the proof result as an artifact rather than
// terminal scrollback. External input values never appear here, only names.
type Verdict struct {
	Version  int             `yaml:"version"`
	Created  string          `yaml:"created"`
	Tool     string          `yaml:"tool"`
	Manifest string          `yaml:"manifest"`
	Refresh  bool            `yaml:"refresh"`
	OK       bool            `yaml:"ok"`
	Order    []string        `yaml:"order"`
	Modules  []ModuleVerdict `yaml:"modules"`
	// ExternalInputs lists the names of externally supplied inputs (values are
	// deliberately not recorded).
	ExternalInputs []string `yaml:"external_inputs,omitempty"`
}

// WriteVerdict marshals the verdict into rootDir and returns its path.
func WriteVerdict(v *Verdict, rootDir string, created time.Time) (string, error) {
	b, err := yaml.Marshal(v)
	if err != nil {
		return "", err
	}
	path := filepath.Join(rootDir, VerdictPrefix+created.UTC().Format(TimeLayout)+".yaml")
	return path, os.WriteFile(path, b, 0o644)
}
