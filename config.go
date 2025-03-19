package main

import (
    "fmt"
    "io/ioutil"
    "gopkg.in/yaml.v2"
    "crypto/md5"
	"encoding/hex"
)
type Device struct {        
    URL string `yaml:"url"`
    ID string
    Audio string `yaml:"audio"`
    Video string `yaml:"video"`
}

type MQTT struct {
    URL      string `yaml:"url"`
    Username string `yaml:"username"`
    Password string `yaml:"password"`
    Name string `yaml:"name"`
}

type Config struct {    
    MQTT MQTT `yaml:"mqtt"`
    Device Device `yaml:"device"`
}

func loadConfig(filename string) (*Config, error) {
    data, err := ioutil.ReadFile(filename)
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

    return &config, nil
}