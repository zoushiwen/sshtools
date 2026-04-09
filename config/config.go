package config

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
)

const defaultSSHTimeout = 15 * time.Second
const (
	DefaultGroupName = "未分组"
	DefaultGroupTag  = "default"
)

//go:embed default.yaml
var embeddedDefaultConfig []byte

type IPSelection string

const (
	IPSelectionPrompt   IPSelection = ""
	IPSelectionPublic   IPSelection = "public"
	IPSelectionIntranet IPSelection = "intranet"
)

type Machine struct {
	Name                 string `mapstructure:"name"`
	IntranetIP           string `mapstructure:"intranet_ip"`
	PublicIP             string `mapstructure:"public_ip"`
	Port                 int    `mapstructure:"port"`
	User                 string `mapstructure:"user"`
	Password             string `mapstructure:"password"`
	PrivateKeyPath       string `mapstructure:"private_key_path"`
	PrivateKeyPassphrase string `mapstructure:"private_key_passphrase"`
	Platform             string `mapstructure:"platform"`
}

type Group struct {
	Name     string    `mapstructure:"name"`
	Tag      string    `mapstructure:"tag"`
	Machines []Machine `mapstructure:"machines"`
}

type Config struct {
	Groups               []Group
	Machines             []Machine
	SSHTimeout           time.Duration
	DefaultIPType        IPSelection
	SourcePath           string
	UsingEmbeddedDefault bool
}

type IndexedMachine struct {
	ID        int
	GroupName string
	GroupTag  string
	Machine   Machine
}

type GroupSummary struct {
	ID           int
	Name         string
	Tag          string
	MachineCount int
}

type rawConfig struct {
	Groups            []Group   `mapstructure:"groups"`
	Machines          []Machine `mapstructure:"machines"`
	SSHTimeout        string    `mapstructure:"ssh_timeout"`
	SSHTimeoutSeconds int       `mapstructure:"ssh_timeout_seconds"`
	DefaultIPType     string    `mapstructure:"default_ip_type"`
}

func Load(explicitPath string) (*Config, error) {
	source, err := resolveConfigSource(explicitPath)
	if err != nil {
		return nil, err
	}

	v := viper.New()
	v.SetEnvPrefix("SSHTOOL")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if source.Embedded {
		v.SetConfigType("yaml")
		if err := v.ReadConfig(bytes.NewReader(source.Content)); err != nil {
			return nil, fmt.Errorf("读取内嵌默认配置失败: %w", err)
		}
	} else {
		v.SetConfigFile(source.Path)
		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("读取配置文件失败: %w", err)
		}
	}

	var raw rawConfig
	if err := v.Unmarshal(&raw); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	cfg, err := normalize(raw, source.ConfigDir)
	if err != nil {
		return nil, err
	}

	cfg.SourcePath = source.Path
	cfg.UsingEmbeddedDefault = source.Embedded
	return cfg, nil
}

type configSource struct {
	Path      string
	ConfigDir string
	Content   []byte
	Embedded  bool
}

func normalize(raw rawConfig, configDir string) (*Config, error) {
	if len(raw.Groups) == 0 && len(raw.Machines) == 0 {
		return nil, errors.New("配置文件中未定义任何机器，请检查 groups 或 machines 字段")
	}

	defaultIPType, err := normalizeIPSelection(raw.DefaultIPType)
	if err != nil {
		return nil, err
	}

	seenNames := make(map[string]string)
	seenTags := make(map[string]string)
	groups := make([]Group, 0, len(raw.Groups)+1)
	machines := make([]Machine, 0)

	for groupIdx, group := range raw.Groups {
		location := fmt.Sprintf("groups[%d]", groupIdx)
		normalizedGroup, err := normalizeGroup(location, group, configDir, seenTags, seenNames)
		if err != nil {
			return nil, err
		}

		groups = append(groups, normalizedGroup)
		machines = append(machines, normalizedGroup.Machines...)
	}

	if len(raw.Machines) > 0 {
		defaultGroup := Group{
			Name:     DefaultGroupName,
			Tag:      DefaultGroupTag,
			Machines: raw.Machines,
		}
		normalizedGroup, err := normalizeGroup("machines", defaultGroup, configDir, seenTags, seenNames)
		if err != nil {
			return nil, err
		}

		groups = append(groups, normalizedGroup)
		machines = append(machines, normalizedGroup.Machines...)
	}

	if len(machines) == 0 {
		return nil, errors.New("配置文件中未定义任何机器，请检查 groups 或 machines 字段")
	}

	return &Config{
		Groups:        groups,
		Machines:      machines,
		SSHTimeout:    resolveTimeout(raw),
		DefaultIPType: defaultIPType,
	}, nil
}

func normalizeGroup(location string, group Group, configDir string, seenTags map[string]string, seenNames map[string]string) (Group, error) {
	group.Name = strings.TrimSpace(group.Name)
	group.Tag = strings.TrimSpace(group.Tag)

	if group.Name == "" {
		return Group{}, fmt.Errorf("%s 缺少 name 字段", location)
	}
	if group.Tag == "" {
		return Group{}, fmt.Errorf("%s 缺少 tag 字段", location)
	}

	normalizedTag := strings.ToLower(group.Tag)
	if previous, exists := seenTags[normalizedTag]; exists {
		return Group{}, fmt.Errorf("分组 tag %q 重复，出现于 %s 和 %s", group.Tag, previous, location)
	}
	seenTags[normalizedTag] = location

	normalizedMachines := make([]Machine, 0, len(group.Machines))
	for machineIdx, machine := range group.Machines {
		machineLocation := fmt.Sprintf("%s.machines[%d]", location, machineIdx)
		machineName := strings.TrimSpace(machine.Name)
		machine, err := normalizeMachine(machine, configDir)
		if err != nil {
			return Group{}, fmt.Errorf("主机 %q 配置无效: %w", machineName, err)
		}

		if err := validateMachine(machineLocation, machine, seenNames); err != nil {
			return Group{}, err
		}

		normalizedMachines = append(normalizedMachines, machine)
	}

	group.Machines = normalizedMachines
	return group, nil
}

func normalizeMachine(machine Machine, configDir string) (Machine, error) {
	if machine.Port == 0 {
		machine.Port = 22
	}

	machine.Name = strings.TrimSpace(machine.Name)
	machine.IntranetIP = strings.TrimSpace(machine.IntranetIP)
	machine.PublicIP = strings.TrimSpace(machine.PublicIP)
	machine.User = strings.TrimSpace(machine.User)
	machine.Platform = strings.TrimSpace(machine.Platform)

	privateKeyPath, err := resolvePrivateKeyPath(configDir, machine.PrivateKeyPath)
	if err != nil {
		return Machine{}, err
	}
	machine.PrivateKeyPath = privateKeyPath

	return machine, nil
}

func validateMachine(location string, machine Machine, seenNames map[string]string) error {
	name := strings.TrimSpace(machine.Name)
	if name == "" {
		return fmt.Errorf("%s 缺少 name 字段", location)
	}
	if previous, exists := seenNames[strings.ToLower(name)]; exists {
		return fmt.Errorf("主机名 %q 重复，出现于 %s 和 %s", name, previous, location)
	}
	seenNames[strings.ToLower(name)] = location

	if strings.TrimSpace(machine.IntranetIP) == "" && strings.TrimSpace(machine.PublicIP) == "" {
		return fmt.Errorf("主机 %q 至少需要配置 intranet_ip 或 public_ip", name)
	}
	if machine.Port < 0 || machine.Port > 65535 {
		return fmt.Errorf("主机 %q 的 port 非法: %d", name, machine.Port)
	}
	if strings.TrimSpace(machine.User) == "" {
		return fmt.Errorf("主机 %q 缺少 user 字段", name)
	}
	if strings.TrimSpace(machine.Password) == "" && strings.TrimSpace(machine.PrivateKeyPath) == "" {
		return fmt.Errorf("主机 %q 至少需要配置 password 或 private_key_path", name)
	}

	return nil
}

func normalizeIPSelection(raw string) (IPSelection, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "prompt", "manual", "ask":
		return IPSelectionPrompt, nil
	case "public", "public_ip", "public-ip", "external", "公网", "公网ip", "外网", "外网ip":
		return IPSelectionPublic, nil
	case "intranet", "intranet_ip", "intranet-ip", "internal", "private", "内网", "内网ip":
		return IPSelectionIntranet, nil
	default:
		return "", fmt.Errorf("default_ip_type 配置无效: %q，仅支持 public 或 intranet", raw)
	}
}

func resolvePrivateKeyPath(configDir, rawPath string) (string, error) {
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" {
		return "", nil
	}

	resolved := trimmed
	if trimmed == "~" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("解析 private_key_path 失败: %w", err)
		}
		resolved = homeDir
	} else if strings.HasPrefix(trimmed, "~/") {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("解析 private_key_path 失败: %w", err)
		}
		resolved = filepath.Join(homeDir, strings.TrimPrefix(trimmed, "~/"))
	}
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(configDir, resolved)
	}

	absPath, err := filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("解析 private_key_path 失败: %w", err)
	}

	info, err := os.Stat(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("private_key_path 不存在: %s", absPath)
		}
		return "", fmt.Errorf("无法访问 private_key_path %s: %w", absPath, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("private_key_path 不能是目录: %s", absPath)
	}

	return absPath, nil
}

func resolveTimeout(raw rawConfig) time.Duration {
	if strings.TrimSpace(raw.SSHTimeout) != "" {
		if duration, err := time.ParseDuration(strings.TrimSpace(raw.SSHTimeout)); err == nil && duration > 0 {
			return duration
		}

		if seconds, err := strconv.Atoi(strings.TrimSpace(raw.SSHTimeout)); err == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
	}

	if raw.SSHTimeoutSeconds > 0 {
		return time.Duration(raw.SSHTimeoutSeconds) * time.Second
	}

	return defaultSSHTimeout
}

func resolveConfigSource(explicitPath string) (configSource, error) {
	if strings.TrimSpace(explicitPath) != "" {
		absPath, err := filepath.Abs(strings.TrimSpace(explicitPath))
		if err != nil {
			return configSource{}, fmt.Errorf("解析配置文件路径失败: %w", err)
		}
		if _, err := os.Stat(absPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return configSource{}, fmt.Errorf("指定的配置文件不存在: %s", absPath)
			}
			return configSource{}, fmt.Errorf("无法访问配置文件 %s: %w", absPath, err)
		}
		return configSource{
			Path:      absPath,
			ConfigDir: filepath.Dir(absPath),
		}, nil
	}

	workingDir, err := os.Getwd()
	if err != nil {
		return configSource{}, fmt.Errorf("获取当前目录失败: %w", err)
	}

	candidates := []string{
		filepath.Join(workingDir, "config.yaml"),
	}

	homeDir, err := os.UserHomeDir()
	if err == nil {
		candidates = append(candidates, filepath.Join(homeDir, ".ssh-tool", "config.yaml"))
	}

	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			absPath, absErr := filepath.Abs(candidate)
			if absErr != nil {
				return configSource{}, fmt.Errorf("解析配置文件路径失败: %w", absErr)
			}
			return configSource{
				Path:      absPath,
				ConfigDir: filepath.Dir(absPath),
			}, nil
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return configSource{}, fmt.Errorf("无法访问配置文件 %s: %w", candidate, err)
		}
	}

	return configSource{
		Path:      "内嵌 default.yaml",
		ConfigDir: workingDir,
		Content:   embeddedDefaultConfig,
		Embedded:  true,
	}, nil
}

func (c *Config) IndexedMachines() []IndexedMachine {
	items := make([]IndexedMachine, 0, len(c.Machines))
	id := 1
	for _, group := range c.Groups {
		for _, machine := range group.Machines {
			items = append(items, IndexedMachine{
				ID:        id,
				GroupName: group.Name,
				GroupTag:  group.Tag,
				Machine:   machine,
			})
			id++
		}
	}
	return items
}

func (c *Config) FindByID(id int) (IndexedMachine, bool) {
	for _, item := range c.IndexedMachines() {
		if item.ID == id {
			return item, true
		}
	}
	return IndexedMachine{}, false
}

func (c *Config) Search(query string, exact bool) []IndexedMachine {
	query = strings.TrimSpace(strings.ToLower(query))
	if query == "" {
		return nil
	}

	matches := make([]IndexedMachine, 0)
	for _, item := range c.IndexedMachines() {
		values := []string{
			strings.ToLower(item.Machine.Name),
			strings.ToLower(item.Machine.IntranetIP),
			strings.ToLower(item.Machine.PublicIP),
			strings.ToLower(item.Machine.User),
			strings.ToLower(item.Machine.Platform),
			strconv.Itoa(item.Machine.Port),
			strings.ToLower(item.GroupName),
			strings.ToLower(item.GroupTag),
		}

		matched := false
		for _, value := range values {
			if value == "" {
				continue
			}

			if exact && value == query {
				matched = true
				break
			}
			if !exact && strings.Contains(value, query) {
				matched = true
				break
			}
		}

		if matched {
			matches = append(matches, item)
		}
	}

	return matches
}

func (c *Config) GroupSummaries() []GroupSummary {
	summaries := make([]GroupSummary, 0, len(c.Groups))
	for idx, group := range c.Groups {
		summaries = append(summaries, GroupSummary{
			ID:           idx + 1,
			Name:         group.Name,
			Tag:          group.Tag,
			MachineCount: len(group.Machines),
		})
	}
	return summaries
}

func (c *Config) FindGroupByTag(tag string) (Group, bool) {
	normalizedTag := strings.ToLower(strings.TrimSpace(tag))
	if normalizedTag == "" {
		return Group{}, false
	}

	for _, group := range c.Groups {
		if strings.ToLower(group.Tag) == normalizedTag {
			return group, true
		}
	}

	return Group{}, false
}

func (c *Config) IndexedMachinesByTag(tag string) ([]IndexedMachine, bool) {
	group, ok := c.FindGroupByTag(tag)
	if !ok {
		return nil, false
	}

	items := make([]IndexedMachine, 0, len(group.Machines))
	for _, item := range c.IndexedMachines() {
		if strings.EqualFold(item.GroupTag, group.Tag) {
			items = append(items, item)
		}
	}

	return items, true
}
