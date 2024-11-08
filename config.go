package main

import (
    "fmt"
    "io/ioutil"
    "gopkg.in/yaml.v2"
)

type Config struct {    
    MQTT struct {
        URL      string `yaml:"url"`
        Username string `yaml:"username"`
        Password string `yaml:"password"`
        Name string `yaml:"name"`

    } `yaml:"mqtt"`
    Device struct {
        ID string `yaml:"id"`
        URL string `yaml:"url"`
    } `yaml:"device"`
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

    return &config, nil
}