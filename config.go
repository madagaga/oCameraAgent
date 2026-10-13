package main

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v2"
)

type Device struct {
	URL   string `yaml:"url"`
	ID    string
	Audio string `yaml:"audio"`
	Video string `yaml:"video"`
}

type MQTT struct {
	URL      string `yaml:"url"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	Name     string `yaml:"name"`
}

type Config struct {
	MQTT   MQTT   `yaml:"mqtt"`
	Device Device `yaml:"device"`
}

func loadConfig(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("erreur de lecture du fichier de configuration : %w", err)
	}

	var config Config
	err = yaml.Unmarshal(data, &config)
	if err != nil {
		return nil, fmt.Errorf("erreur de parsing du fichier de configuration : %w", err)
	}

	// set device id with md5 of url
	hash := md5.Sum([]byte(config.Device.URL))
	config.Device.ID = hex.EncodeToString(hash[:])

	if err := config.Validate(); err != nil {
		return nil, err
	}

	return &config, nil
}

func (c *Config) Validate() error {
	if strings.TrimSpace(c.Device.URL) == "" {
		return fmt.Errorf("configuration invalide: device.url est requis")
	}
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(c.Device.URL)), "rtsp://") {
		return fmt.Errorf("configuration invalide: device.url doit commencer par rtsp://")
	}

	if strings.TrimSpace(c.Device.Video) == "" {
		return fmt.Errorf("configuration invalide: device.video est requis (ex: H264)")
	}
	if strings.TrimSpace(c.Device.Audio) == "" {
		return fmt.Errorf("configuration invalide: device.audio est requis (ex: PCMU)")
	}

	if strings.TrimSpace(c.MQTT.URL) == "" {
		return fmt.Errorf("configuration invalide: mqtt.url est requis")
	}
	if strings.TrimSpace(c.MQTT.Name) == "" {
		return fmt.Errorf("configuration invalide: mqtt.name est requis")
	}

	username := strings.TrimSpace(c.MQTT.Username)
	password := strings.TrimSpace(c.MQTT.Password)
	if (username == "") != (password == "") {
		return fmt.Errorf("configuration invalide: mqtt.username et mqtt.password doivent être tous les deux renseignés ou tous les deux vides")
	}

	return nil
}
