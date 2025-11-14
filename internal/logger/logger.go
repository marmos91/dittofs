package logger

import (
	"fmt"
	stdlog "log"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

var (
	currentLevel atomic.Int32
	logger       = stdlog.New(os.Stdout, "", 0)
)

func init() {
	// Set default level to Info
	currentLevel.Store(int32(LevelInfo))
}

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}

func SetLevel(level string) {
	switch strings.ToUpper(level) {
	case "DEBUG":
		currentLevel.Store(int32(LevelDebug))
	case "INFO":
		currentLevel.Store(int32(LevelInfo))
	case "WARN":
		currentLevel.Store(int32(LevelWarn))
	case "ERROR":
		currentLevel.Store(int32(LevelError))
	}
}

func log(level Level, format string, v ...any) {
	if level < Level(currentLevel.Load()) {
		return
	}

	timestamp := time.Now().Format("2006-01-02 15:04:05")
	prefix := fmt.Sprintf("[%s] [%s] ", timestamp, level.String())
	message := fmt.Sprintf(format, v...)
	logger.Println(prefix + message)
}

func Debug(format string, v ...any) {
	log(LevelDebug, format, v...)
}

func Info(format string, v ...any) {
	log(LevelInfo, format, v...)
}

func Warn(format string, v ...any) {
	log(LevelWarn, format, v...)
}

func Error(format string, v ...any) {
	log(LevelError, format, v...)
}
