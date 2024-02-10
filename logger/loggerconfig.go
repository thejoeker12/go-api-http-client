// logger.go
package logger

// Ref: https://betterstack.com/community/guides/logging/go/zap/#logging-errors-with-zap

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	LogOutputJSON          = "json"
	LogOutputHumanReadable = "human-readable"
)

// BuildLogger creates and returns a new zap logger instance.
// It configures the logger with JSON formatting and a custom encoder to ensure the 'pid', 'application', and 'timestamp' fields
// appear at the end of each log message. The function panics if the logger cannot be initialized.
func BuildLogger(logLevel LogLevel, logOutputFormat string) Logger {

	// Set up custom encoder configuration
	encoderCfg := zap.NewProductionEncoderConfig()
	encoderCfg.TimeKey = "timestamp"
	encoderCfg.EncodeTime = zapcore.RFC3339TimeEncoder        // ISO8601TimeEncoder /Use ISO8601 format for timestamps
	encoderCfg.EncodeLevel = zapcore.CapitalColorLevelEncoder // Encodes the level in uppercase (e.g., "INFO", "ERROR") with ANSI colour.
	encoderCfg.EncodeCaller = zapcore.ShortCallerEncoder      // Encodes the file and line number of the caller if the log level is at least DebugLevel
	encoderCfg.EncodeName = zapcore.FullNameEncoder           // Encodes the logger name

	// Convert the custom LogLevel to zap's logging level
	zapLogLevel := convertToZapLevel(logLevel)

	// Define the logger configuration
	config := zap.Config{
		Level:             zap.NewAtomicLevelAt(zapLogLevel), // Default log level is Info
		Development:       false,                             // Set to true if the logger is used in a development environment
		Encoding:          "console",                         // Supports 'json' and 'console' encodings
		DisableCaller:     true,
		DisableStacktrace: true,
		Sampling:          nil,
		EncoderConfig:     encoderCfg,
		OutputPaths: []string{
			"stdout", // Log info and above to standard output
		},
		ErrorOutputPaths: []string{ // is similar to OutputPaths but is used for Zap's internal errors only, not those generated or logged by your application (such as the error from mismatched loosely-typed key/value pairs).
			"stderr", // Log internal Zap errors to standard error
		},
		InitialFields: map[string]interface{}{ // specifies global contextual fields that should be included in every log entry produced by each logger created from the Config object
			//"pid":         os.Getpid(),
			//"application": version.GetAppName(),
		},
	}
	// Build the logger from the configuration
	logger := zap.Must(config.Build())

	// Wrap the original core with the custom core
	wrappedCore := &customCore{logger.Core()}
	wrappedLogger := zap.New(wrappedCore)

	// Wrap the Zap logger in your defaultLogger struct, which implements the Logger interface
	return &defaultLogger{
		logger:   wrappedLogger,
		logLevel: logLevel,
	}
}

// convertToZapLevel converts the custom LogLevel to a zapcore.Level
func convertToZapLevel(level LogLevel) zapcore.Level {
	switch level {
	case LogLevelDebug:
		return zap.DebugLevel
	case LogLevelInfo:
		return zap.InfoLevel
	case LogLevelWarn:
		return zap.WarnLevel
	case LogLevelError:
		return zap.ErrorLevel
	case LogLevelDPanic:
		return zap.DPanicLevel
	case LogLevelPanic:
		return zap.PanicLevel
	case LogLevelFatal:
		return zap.FatalLevel
	default:
		return zap.InfoLevel // Default to InfoLevel
	}
}
