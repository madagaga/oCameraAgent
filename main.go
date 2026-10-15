package main

import (
	"flag"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	configFile := flag.String("config", "config.yaml", "config file path")
	debug := flag.Bool("debug", false, "enable debug logging")
	flag.Parse()

	// Initialize logger
	InitLogger(*debug)
	Info("MAIN", "Starting oCamera agent %s (protocol v%d)...", AgentVersion, ProtocolVersion)

	config, err := loadConfig(*configFile)
	if err != nil {
		Fatal("MAIN", "Error while loading config: %v", err)
	}
	Info("MAIN", "Camera id=%s name=%q", config.ID(), config.Device.Name)

	agent, err := NewAgent(config)
	if err != nil {
		Fatal("MAIN", "Error while creating agent: %v", err)
	}
	if err := agent.Run(); err != nil {
		Fatal("MAIN", "MQTT connection error: %v", err)
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	<-c

	Info("MAIN", "Closing app...")
	agent.Shutdown()
}
