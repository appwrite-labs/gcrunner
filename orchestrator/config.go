package function

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	"gopkg.in/yaml.v3"
)

// configPath is where a repository keeps its runner presets and images.
const configPath = ".github/gcrunner.yml"

// errConfiguration marks a mistake in the workflow or the config file. A retry
// cannot mend it, so the job is left queued with the reason logged.
var errConfiguration = errors.New("configuration error")

// errConfigForbidden reports an App installation that has not been granted
// read access to repository contents, so the file cannot be read at all.
var errConfigForbidden = errors.New("the gcrunner GitHub App has no read access to repository contents")

// RepositoryConfig is the parsed .github/gcrunner.yml. A repository without
// one gets the zero value, which resolves every job from its labels alone.
type RepositoryConfig struct {
	Runners map[string]RunnerPreset    `yaml:"runners"`
	Images  map[string]ImageDefinition `yaml:"images"`

	// unreadable is set when the file could not be read for lack of
	// permission, so a job that depends on it is held rather than guessed at.
	unreadable bool
}

// RunnerPreset is a named set of runner settings a job selects with runner=.
// Every field is optional and uses the label of the same name.
type RunnerPreset struct {
	Machine  string `yaml:"machine"`
	Family   Joined `yaml:"family"`
	CPU      Joined `yaml:"cpu"`
	RAM      Joined `yaml:"ram"`
	Spot     *bool  `yaml:"spot"`
	Disk     string `yaml:"disk"`
	DiskType string `yaml:"disk-type"`
	Image    string `yaml:"image"`
	Zone     Joined `yaml:"zone"`
}

// ImageDefinition names a GCE image a job selects with image=<name>. Exactly
// one of Family and Name is set; Project defaults to the deployment's image
// project.
type ImageDefinition struct {
	Project string `yaml:"project"`
	Family  string `yaml:"family"`
	Name    string `yaml:"name"`
}

var (
	rangePattern = regexp.MustCompile(`^[0-9]+(\+[0-9]+)?$`)
	diskPattern  = regexp.MustCompile(`(?i)^[0-9]+(gb)?$`)
)

// validate rejects values the labels would misread, so a typo holds the job
// with a message instead of provisioning a machine nobody asked for.
func (p RunnerPreset) validate() error {
	for key, value := range map[string]string{labelCPU: string(p.CPU), labelRAM: string(p.RAM)} {
		if value != "" && !rangePattern.MatchString(value) {
			return fmt.Errorf("%s: %q is not a count or a min+max range", key, value)
		}
	}
	if p.Disk != "" && !diskPattern.MatchString(p.Disk) {
		return fmt.Errorf("%s: %q is not a size like 100gb", labelDisk, p.Disk)
	}
	return nil
}

// Joined is a label value that YAML may write as a scalar or a list. A list
// joins with "+", so family: [n2, c3] is family=n2+c3 and cpu: [4, 16] is
// cpu=4+16.
type Joined string

func (j *Joined) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		*j = Joined(node.Value)
		return nil
	case yaml.SequenceNode:
		values := make([]string, 0, len(node.Content))
		for _, item := range node.Content {
			if item.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: expected a scalar list item", item.Line)
			}
			values = append(values, item.Value)
		}
		*j = Joined(strings.Join(values, "+"))
		return nil
	}
	return fmt.Errorf("line %d: expected a scalar or a list", node.Line)
}

// settings converts the preset into the label vocabulary, keeping only what
// the preset set so it layers cleanly between the defaults and the job.
func (p RunnerPreset) settings() Settings {
	settings := Settings{}
	for key, value := range map[string]string{
		labelMachine:  p.Machine,
		labelFamily:   string(p.Family),
		labelCPU:      string(p.CPU),
		labelRAM:      string(p.RAM),
		labelDisk:     p.Disk,
		labelDiskType: p.DiskType,
		labelImage:    p.Image,
		labelZone:     string(p.Zone),
	} {
		if value != "" {
			settings[key] = value
		}
	}
	if p.Spot != nil {
		settings[labelSpot] = strconv.FormatBool(*p.Spot)
	}
	return settings
}

// path is the GCE source image this definition points at.
func (d ImageDefinition) path(defaultProject string) (string, error) {
	project := d.Project
	if project == "" {
		project = defaultProject
	}
	switch {
	case d.Family != "" && d.Name != "":
		return "", fmt.Errorf("set either family or name, not both")
	case d.Family != "":
		return fmt.Sprintf("projects/%s/global/images/family/%s", project, d.Family), nil
	case d.Name != "":
		return fmt.Sprintf("projects/%s/global/images/%s", project, d.Name), nil
	}
	return "", fmt.Errorf("set family or name")
}

// resolve turns a job's labels into a runner, layering its preset underneath
// its own labels and swapping a configured image name for the image it names.
// An error here is the workflow author's to fix, not something a retry mends.
func (c *RepositoryConfig) resolve(job *JobLabels, imageProject string) (*RunnerLabels, error) {
	var preset Settings
	if job.Runner != "" {
		if c.unreadable {
			return nil, fmt.Errorf("%w: runner %q needs %s, but %v", errConfiguration, job.Runner, configPath, errConfigForbidden)
		}
		definition, ok := c.Runners[job.Runner]
		if !ok {
			return nil, fmt.Errorf("%w: runner %q is not defined in %s", errConfiguration, job.Runner, configPath)
		}
		preset = definition.settings()
	}
	runner := job.runner(preset)
	if definition, ok := c.Images[runner.Image]; ok {
		path, err := definition.path(imageProject)
		if err != nil {
			return nil, fmt.Errorf("%w: image %q in %s: %v", errConfiguration, runner.Image, configPath, err)
		}
		runner.Image = path
	}
	return runner, nil
}

// parseRepositoryConfig decodes the file, refusing keys it does not know so a
// misspelt setting is reported rather than dropped.
func parseRepositoryConfig(data []byte) (*RepositoryConfig, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var config RepositoryConfig
	if err := decoder.Decode(&config); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: parse %s: %v", errConfiguration, configPath, err)
	}
	for name, preset := range config.Runners {
		if err := preset.validate(); err != nil {
			return nil, fmt.Errorf("%w: runner %q in %s: %v", errConfiguration, name, configPath, err)
		}
	}
	return &config, nil
}

// configRef is the commit the config is read from. A private repository reads
// the file at the job's own commit, so a branch can change its runners. A
// public repository reads the default branch only, so a pull request from a
// fork cannot pick its own machines.
func configRef(event WorkflowJobEvent) string {
	if event.Repository.Private {
		return event.WorkflowJob.HeadSHA
	}
	return event.Repository.DefaultBranch
}

// ConfigCache keeps parsed configs by commit. Only immutable refs are cached;
// a branch name is read fresh so an edit to the file shows up on the next job.
// Concurrent misses for one key share a single fetch, so a workflow fanning
// out twenty jobs at once reads the file once.
type ConfigCache struct {
	mu      sync.RWMutex
	configs map[string]configCacheEntry
	ttl     time.Duration
	nowFunc func() time.Time
	group   singleflight.Group
}

type configCacheEntry struct {
	config    *RepositoryConfig
	fetchedAt time.Time
}

var configCache = &ConfigCache{
	configs: make(map[string]configCacheEntry),
	ttl:     1 * time.Hour,
	nowFunc: time.Now,
}

// fetchRepositoryFile is a seam so loading can be exercised without GitHub.
var fetchRepositoryFile = fetchRepositoryContents

// loadRepositoryConfig reads the repository's .github/gcrunner.yml for this
// job. A missing file yields an empty config. An App installation that has not
// been granted contents access yet yields a config marked unreadable, so jobs
// that never reference the file keep running while jobs that select a runner
// are held until the permission is accepted.
func loadRepositoryConfig(ctx context.Context, event WorkflowJobEvent) (*RepositoryConfig, error) {
	owner := event.Repository.Owner.Login
	repo := event.Repository.Name
	ref := configRef(event)
	config, err := configCache.load(ctx, owner, repo, ref, isCommitSHA(ref))
	if errors.Is(err, errConfigForbidden) {
		log.Printf("Cannot read %s in %s/%s: %v", configPath, owner, repo, err)
		return &RepositoryConfig{unreadable: true}, nil
	}
	return config, err
}

// load returns the parsed file at ref. A permission failure is never cached,
// so the first job after an administrator accepts the permission reads the
// file.
func (c *ConfigCache) load(ctx context.Context, owner, repo, ref string, cacheable bool) (*RepositoryConfig, error) {
	key := owner + "/" + repo + "@" + ref
	now := c.nowFunc()

	if cacheable {
		c.mu.RLock()
		entry, ok := c.configs[key]
		c.mu.RUnlock()
		if ok && now.Sub(entry.fetchedAt) < c.ttl {
			return entry.config, nil
		}
	}

	// The fetch is shared by every caller waiting on this key, so it must
	// outlive the first caller's request: a cancelled webhook would otherwise
	// fail every job that happened to arrive alongside it.
	fetchCtx := context.WithoutCancel(ctx)
	result, err, _ := c.group.Do(key, func() (any, error) {
		data, err := fetchRepositoryFile(fetchCtx, owner, repo, configPath, ref)
		var config *RepositoryConfig
		switch {
		case err == nil && data == nil:
			config = &RepositoryConfig{}
		case err == nil:
			config, err = parseRepositoryConfig(data)
			if err != nil {
				return nil, err
			}
		case isForbidden(err):
			return nil, fmt.Errorf("%w: %v", errConfigForbidden, err)
		default:
			return nil, fmt.Errorf("read %s in %s/%s at %s: %w", configPath, owner, repo, ref, err)
		}

		if cacheable {
			c.mu.Lock()
			c.evictExpired(now)
			c.configs[key] = configCacheEntry{config: config, fetchedAt: now}
			c.mu.Unlock()
		}
		return config, nil
	})
	if err != nil {
		return nil, err
	}
	return result.(*RepositoryConfig), nil
}

// evictExpired drops entries past their TTL so a busy repository does not
// grow the map by one entry per commit for the instance's lifetime. Called
// with the write lock held.
func (c *ConfigCache) evictExpired(now time.Time) {
	for key, entry := range c.configs {
		if now.Sub(entry.fetchedAt) >= c.ttl {
			delete(c.configs, key)
		}
	}
}

func isCommitSHA(ref string) bool {
	if len(ref) != 40 {
		return false
	}
	for _, r := range ref {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}
