package function

import "strings"

const (
	labelRunID    = "gcrunner"
	labelRunner   = "runner"
	labelMachine  = "machine"
	labelFamily   = "family"
	labelSpot     = "spot"
	labelDisk     = "disk"
	labelDiskType = "disk-type"
	labelImage    = "image"
	labelCPU      = "cpu"
	labelRAM      = "ram"
	labelZone     = "zone"
)

const (
	machineModeExact  = "exact"
	machineModeFamily = "family"
	machineModeAuto   = "auto"
)

// Settings are runner options keyed by label name, holding only what was
// spelled out, so a layer can tell a default apart from an explicit choice.
type Settings map[string]string

var defaultSettings = Settings{
	labelMachine:  "n2d-standard-2",
	labelSpot:     "true",
	labelDisk:     "75gb",
	labelDiskType: "pd-ssd",
	labelImage:    "ubuntu24-full-x64",
}

// RunnerLabels holds the resolved gcrunner configuration for a job.
type RunnerLabels struct {
	RunID    string
	Machine  string // Exact machine type (e.g. "n2d-standard-4", "e2-micro")
	Family   string // Machine family for resolution (e.g. "n2d", "n2d+c3")
	Spot     bool
	Disk     string
	DiskType string
	Image    string
	CPU      string // "4" or "2+8" (range)
	RAM      string // "16" or "8+32" (range)
	Zone     string // "us-central1-a" or "us-central1-a+us-central1-b"
	// MachineMode is computed after parsing:
	//   "exact"  — Machine is set, use as-is
	//   "family" — Family is set, resolve with cpu/ram constraints
	//   "auto"   — Only cpu/ram set, resolve using default family (n2d)
	MachineMode string
}

// JobLabels is what a job wrote on its gcrunner label: the run it belongs to,
// the preset it selected with runner=, and the settings it set explicitly.
type JobLabels struct {
	RunID    string
	Runner   string
	Settings Settings
}

// parseJobLabels finds the gcrunner label among a job's labels.
// Returns nil if this is not a gcrunner job.
func parseJobLabels(labels []string) *JobLabels {
	for _, label := range labels {
		if !strings.HasPrefix(label, labelRunID+"=") {
			continue
		}
		job := &JobLabels{Settings: Settings{}}
		for _, part := range strings.Split(label, "/") {
			key, value, ok := strings.Cut(part, "=")
			if !ok {
				continue
			}
			switch key {
			case labelRunID:
				job.RunID = value
			case labelRunner:
				job.Runner = value
			default:
				job.Settings[key] = value
			}
		}
		return job
	}
	return nil
}

// parseLabels extracts gcrunner config from workflow_job labels.
// Returns nil if this is not a gcrunner job.
func parseLabels(labels []string) *RunnerLabels {
	job := parseJobLabels(labels)
	if job == nil {
		return nil
	}
	return job.runner(nil)
}

// runner lays the preset over the defaults and the job's own settings over
// both. Machine mode comes from the explicit layers, and the job's choice of
// how to pick a machine beats the preset's: a job that writes machine= wants
// that machine, so a family= inherited from its preset is dropped rather than
// left to outrank it, and a job that writes cpu= or ram= wants a machine that
// fits, so a machine= inherited from its preset is dropped rather than left
// to make the constraint a no-op.
func (j *JobLabels) runner(preset Settings) *RunnerLabels {
	explicit := merge(preset, j.Settings)
	if j.Settings[labelMachine] != "" && j.Settings[labelFamily] == "" {
		delete(explicit, labelFamily)
	}
	if j.Settings[labelMachine] == "" && (j.Settings[labelCPU] != "" || j.Settings[labelRAM] != "") {
		delete(explicit, labelMachine)
	}
	settings := merge(defaultSettings, explicit)
	return &RunnerLabels{
		RunID:       j.RunID,
		Machine:     settings[labelMachine],
		Family:      settings[labelFamily],
		Spot:        settings[labelSpot] != "false",
		Disk:        settings[labelDisk],
		DiskType:    settings[labelDiskType],
		Image:       settings[labelImage],
		CPU:         settings[labelCPU],
		RAM:         settings[labelRAM],
		Zone:        settings[labelZone],
		MachineMode: classifyMachineMode(explicit),
	}
}

// merge lays each layer over the previous one; later layers win.
func merge(layers ...Settings) Settings {
	merged := Settings{}
	for _, layer := range layers {
		for key, value := range layer {
			merged[key] = value
		}
	}
	return merged
}

// classifyMachineMode determines how the machine type should be resolved from
// what was set explicitly, so a machine= that happens to name the default is
// still honoured as an exact request.
//
// Priority:
//  1. family= is set → "family" mode (resolve using family + cpu/ram)
//  2. machine= is set → "exact" mode
//  3. cpu= or ram= set → "auto" mode (default family n2d)
//  4. Nothing set → "exact" mode with default machine
func classifyMachineMode(explicit Settings) string {
	switch {
	case explicit[labelFamily] != "":
		return machineModeFamily
	case explicit[labelMachine] != "":
		return machineModeExact
	case explicit[labelCPU] != "" || explicit[labelRAM] != "":
		return machineModeAuto
	}
	return machineModeExact
}
