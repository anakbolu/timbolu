package whatsapp

import (
	waLog "go.mau.fi/whatsmeow/util/log"
	"go.uber.org/zap"
)

// WALogger turns the structured logger into whatsmeow's own.
//
// The two are kept apart on purpose. whatsmeow writes a line per decrypted
// message and per protocol frame, which is what somebody debugging a pairing
// wants and what a production log does not: mixing them into the structured
// stream would double its volume and bury the product's own events. It is
// therefore a switch, and this is what the switch turns on.
func WALogger(log *zap.Logger) waLog.Logger {
	if log == nil {
		return waLog.Noop
	}

	return &waLogger{log: log}
}

// waLogger is the adapter.
type waLogger struct {
	log *zap.Logger
}

// Module implements waLog.Logger.
func (w *waLogger) Module(module string) waLog.Logger {
	return &waLogger{log: w.log.Named(module)}
}

// Debugf implements waLog.Logger.
func (w *waLogger) Debugf(msg string, args ...any) {
	w.log.Sugar().Debugf(msg, args...)
}

// Infof implements waLog.Logger.
func (w *waLogger) Infof(msg string, args ...any) {
	w.log.Sugar().Infof(msg, args...)
}

// Warnf implements waLog.Logger.
func (w *waLogger) Warnf(msg string, args ...any) {
	w.log.Sugar().Warnf(msg, args...)
}

// Errorf implements waLog.Logger.
func (w *waLogger) Errorf(msg string, args ...any) {
	w.log.Sugar().Errorf(msg, args...)
}

// Sub implements waLog.Logger.
func (w *waLogger) Sub(module string) waLog.Logger {
	return &waLogger{log: w.log.Named(module)}
}

// Compile-time check that the adapter is what whatsmeow asks for.
var _ waLog.Logger = (*waLogger)(nil)
