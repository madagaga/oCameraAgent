package main

import (
	"fmt"
	"log"
)

// Logger levels
const (
	LevelInfo  = "INFO"
	LevelDebug = "DEBUG"
	LevelError = "ERROR"
	LevelWarn  = "WARN"
)

var (
	// DebugEnabled controls whether debug logs are printed
	DebugEnabled = false
)

// InitLogger initializes the logger with the given debug flag
func InitLogger(debug bool) {
	DebugEnabled = debug
	// Set log flags to include date and time but not the prefix
	// (we'll add our own prefix with the level)
	log.SetFlags(log.Ldate | log.Ltime)
}

// logMessage logs a message with the given level and module
func logMessage(level, module, format string, args ...interface{}) {
	// Skip debug messages if debug is not enabled
	if level == LevelDebug && !DebugEnabled {
		return
	}

	message := fmt.Sprintf(format, args...)
	log.Printf("[%s] [%s] - %s", level, module, message)
}

// Info logs an info level message
func Info(module, format string, args ...interface{}) {
	logMessage(LevelInfo, module, format, args...)
}

// Debug logs a debug level message (only if debug is enabled)
func Debug(module, format string, args ...interface{}) {
	logMessage(LevelDebug, module, format, args...)
}

// Error logs an error level message
func Error(module, format string, args ...interface{}) {
	logMessage(LevelError, module, format, args...)
}

// Warn logs a warning level message
func Warn(module, format string, args ...interface{}) {
	logMessage(LevelWarn, module, format, args...)
}

// Fatal logs an error message and exits the program
func Fatal(module, format string, args ...interface{}) {
	message := fmt.Sprintf(format, args...)
	log.Fatalf("[%s] [%s] - %s", LevelError, module, message)
}
