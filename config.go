package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v2"
)

type Device struct {
	URL   string `yaml:"url"`
	Name  string `yaml:"name"`
	Audio string `yaml:"audio"`
	Video string `yaml:"video"`
}

type MQTT struct {
	URL      string `yaml:"url"`
	Username string `yaml:"username"` // camera identifier = MQTT clientId
	Password string `yaml:"password"`
	CA       string `yaml:"ca"` // PEM file of a custom CA (optional)
}

type ICE struct {
	Stun []string `yaml:"stun"` // STUN servers; default: Google public STUN
}

type Config struct {
	MQTT   MQTT   `yaml:"mqtt"`
	Device Device `yaml:"device"`
	ICE    ICE    `yaml:"ice"`
}

// ID is the camera identifier: the MQTT username (spec § 2).
func (c *Config) ID() string { return strings.TrimSpace(c.MQTT.Username) }

var defaultStun = []string{"stun:stun.l.google.com:19302", "stun:stun1.l.google.com:19302"}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{3,32}$`)

func loadConfig(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("cannot read config file: %w", err)
	}

	var config Config
	err = yaml.Unmarshal(data, &config)
	if err != nil {
		return nil, fmt.Errorf("cannot parse config file: %w", err)
	}

	config.Device.URL = strings.TrimSpace(config.Device.URL)
	config.MQTT.Username = strings.TrimSpace(config.MQTT.Username)
	if strings.TrimSpace(config.Device.Name) == "" {
		config.Device.Name = config.MQTT.Username
	}
	if len(config.ICE.Stun) == 0 {
		config.ICE.Stun = defaultStun
	}

	if err := config.Validate(); err != nil {
		return nil, err
	}

	return &config, nil
}

func (c *Config) Validate() error {
	if c.Device.URL == "" {
		return fmt.Errorf("invalid configuration: device.url is required")
	}
	if !strings.HasPrefix(strings.ToLower(c.Device.URL), "rtsp://") {
		return fmt.Errorf("invalid configuration: device.url must start with rtsp://")
	}

	if strings.TrimSpace(c.Device.Video) == "" {
		return fmt.Errorf("invalid configuration: device.video is required (e.g. H264)")
	}
	if strings.TrimSpace(c.Device.Audio) == "" {
		return fmt.Errorf("invalid configuration: device.audio is required (e.g. PCMU)")
	}
	if len(c.Device.Name) > 64 {
		return fmt.Errorf("invalid configuration: device.name is limited to 64 characters")
	}

	if strings.TrimSpace(c.MQTT.URL) == "" {
		return fmt.Errorf("invalid configuration: mqtt.url is required")
	}
	if !idPattern.MatchString(c.MQTT.Username) {
		return fmt.Errorf("invalid configuration: mqtt.username (camera identifier) must be 3 to 32 characters among [A-Za-z0-9_-]")
	}
	if strings.TrimSpace(c.MQTT.Password) == "" {
		return fmt.Errorf("invalid configuration: mqtt.password is required")
	}

	return nil
}
