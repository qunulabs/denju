package denju

import "log/slog"

// Level is the severity of a line denju emits.
type Level int

// Severity levels, ordered.
const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// String returns the lowercase level name.
func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "debug"
	case LevelInfo:
		return "info"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	default:
		return "unknown"
	}
}

// Logger receives every line denju emits. attrs are alternating key/value pairs,
// in the same variadic form log/slog uses, so an adapter is usually a one-liner.
//
// A nil Logger is valid and means silence. denju does not fall back to
// slog.Default(): a library that writes into a program's global logger without
// being asked is a nuisance, and self-update lines are noisy by nature. Pass
// [SlogLogger] to opt in.
type Logger func(level Level, msg string, attrs ...any)

// SlogLogger adapts a *slog.Logger to [Logger].
func SlogLogger(l *slog.Logger) Logger {
	if l == nil {
		return nil
	}
	return func(level Level, msg string, attrs ...any) {
		switch level {
		case LevelDebug:
			l.Debug(msg, attrs...)
		case LevelWarn:
			l.Warn(msg, attrs...)
		case LevelError:
			l.Error(msg, attrs...)
		default:
			l.Info(msg, attrs...)
		}
	}
}

// log emits a line if the caller supplied a logger.
//
// Every message is prefixed with "self-update: " so that a program which merges
// denju's output into its own log can still tell where a line came from.
func (u *Updater) log(level Level, msg string, attrs ...any) {
	if u.cfg.Log == nil {
		return
	}
	u.cfg.Log(level, "self-update: "+msg, attrs...)
}
