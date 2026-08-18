package manifest

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Sidecar filenames — fixed, like the manifest's: one canonical file per
// step, overwritten per execution, with the datetime (`created`) and the
// generation tie (`manifest_checksum`) inside the document so an external
// system can tell what ran, when, and for which plan. History lives in
// version control.
const (
	MapReceiptFile   = "demonolith-migrate-map.yaml"
	RunReceiptFile   = "demonolith-migrate-run.yaml"
	ProveVerdictFile = "demonolith-migrate-prove.yaml"
	FinalVerdictFile = "demonolith-migrate-verify.yaml"
)

// Receipt actions.
const (
	ActionMap = "map"
	ActionRun  = "run"
)

// Verdict modes.
const (
	ModeProve = "prove"
	ModeFinal = "final"
)

// MoveOutcome is what happened to one state move.
type MoveOutcome struct {
	Address string `yaml:"address"`
	Module  string `yaml:"module"`
	// Outcome is "moved" or "skipped" (already applied).
	Outcome string `yaml:"outcome"`
}

// PushOutcome is what happened to one module's state during migrate run.
type PushOutcome struct {
	Module string `yaml:"module"`
	// Location is the state destination (a derived backend location, or a
	// local path for a backend-less monolith).
	Location string `yaml:"location"`
	// Outcome is "pushed" or "skipped" (already seeded).
	Outcome string `yaml:"outcome"`
}

// Receipt is the execution record of one migrate step: the manifest is the
// plan, the receipt is what actually happened and where the artifacts are.
type Receipt struct {
	Version int    `yaml:"version"`
	Created string `yaml:"created"`
	Tool    string `yaml:"tool"`
	// Manifest is the filename (not path) of the manifest that was executed.
	Manifest string `yaml:"map"`
	// ManifestChecksum is the executed manifest's emit_checksum; the manifest
	// filename is constant, so the checksum is what ties a receipt to one
	// manifest generation.
	ManifestChecksum string `yaml:"map_checksum"`
	// Action is what this receipt records: "map" (the local carve) or "run"
	// (the push into real backends).
	Action string `yaml:"action"`
	Engine string `yaml:"engine,omitempty"`
	// Complete is true when every manifest move is accounted for.
	Complete bool `yaml:"complete"`
	// ModuleStates maps a module to its carved state file, relative to root dir.
	ModuleStates map[string]string `yaml:"module_states"`
	// BackupPath is the pre-carve snapshot of the source state, relative to root dir.
	BackupPath string        `yaml:"backup_path,omitempty"`
	Moves      []MoveOutcome `yaml:"moves,omitempty"`
	// Pushes records where each module's state landed; run receipts only.
	Pushes []PushOutcome `yaml:"pushes,omitempty"`
}

// receiptFile maps a receipt action to its canonical filename.
func receiptFile(action string) string {
	if action == ActionRun {
		return RunReceiptFile
	}
	return MapReceiptFile
}

// WriteReceipt marshals the receipt to its action's canonical path in rootDir
// and returns that path.
func WriteReceipt(r *Receipt, rootDir string) (string, error) {
	b, err := yaml.Marshal(r)
	if err != nil {
		return "", err
	}
	path := filepath.Join(rootDir, receiptFile(r.Action))
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

// LatestReceiptFor loads the canonical receipt for the given action when it
// records the manifest generation with the given emit checksum, or nil when
// absent or from another generation.
func LatestReceiptFor(rootDir, manifestChecksum, action string) (*Receipt, error) {
	path := filepath.Join(rootDir, receiptFile(action))
	if _, err := os.Stat(path); err != nil {
		return nil, nil
	}
	r, err := LoadReceipt(path)
	if err != nil {
		return nil, err
	}
	if r.ManifestChecksum != manifestChecksum || r.Action != action {
		return nil, nil
	}
	return r, nil
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

// Verdict is a proof sidecar: the result as an artifact rather than terminal
// scrollback. Mode "prove" judges migrate map's carved artifacts; mode
// "final" judges the pushed states against the real backends. External input
// values never appear here, only names.
type Verdict struct {
	Version  int    `yaml:"version"`
	Created  string `yaml:"created"`
	Tool     string `yaml:"tool"`
	Manifest string `yaml:"map"`
	// ManifestChecksum ties the verdict to one manifest generation.
	ManifestChecksum string          `yaml:"map_checksum"`
	Mode             string          `yaml:"mode"`
	Refresh          bool            `yaml:"refresh"`
	OK       bool            `yaml:"ok"`
	Order    []string        `yaml:"order"`
	Modules  []ModuleVerdict `yaml:"modules"`
	// ExternalInputs lists the names of externally supplied inputs (values are
	// deliberately not recorded).
	ExternalInputs []string `yaml:"external_inputs,omitempty"`
}

// WriteVerdict marshals the verdict to its mode's canonical path in rootDir
// and returns that path.
func WriteVerdict(v *Verdict, rootDir string) (string, error) {
	b, err := yaml.Marshal(v)
	if err != nil {
		return "", err
	}
	name := ProveVerdictFile
	if v.Mode == ModeFinal {
		name = FinalVerdictFile
	}
	path := filepath.Join(rootDir, name)
	return path, os.WriteFile(path, b, 0o644)
}

// LatestProveVerdict loads the canonical prove verdict when it records the
// given manifest generation, or nil.
func LatestProveVerdict(rootDir, manifestChecksum string) (*Verdict, error) {
	path := filepath.Join(rootDir, ProveVerdictFile)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var v Verdict
	if err := yaml.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("parse verdict %s: %w", path, err)
	}
	if v.ManifestChecksum != manifestChecksum {
		return nil, nil
	}
	return &v, nil
}
