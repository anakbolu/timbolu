package logger

import "os"

// stdout and stderr keep the writer choice in one place so tests can reason
// about where records go.
type stdout struct{}

func (stdout) Write(p []byte) (int, error) { return os.Stdout.Write(p) }

type stderr struct{}

func (stderr) Write(p []byte) (int, error) { return os.Stderr.Write(p) }
