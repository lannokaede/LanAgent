package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

func TestInvalidFlagsAreRejectedBeforeAcceptance(t *testing.T) {
	for _, args := range [][]string{
		{"-classifier-timeout", "0s"},
		{"-classifier-timeout", "31s"},
		{"-mode", "invalid"},
		{"unexpected-positional"},
	} {
		if err := run(context.Background(), args, io.Discard, io.Discard); err == nil {
			t.Fatalf("accepted invalid flags: %v", args)
		}
	}
}

func TestHelpRetainsSupportedFlagNames(t *testing.T) {
	var help bytes.Buffer
	_ = run(context.Background(), []string{"-help"}, io.Discard, &help)
	for _, name := range []string{"mode", "only", "config", "report", "milvus-url", "corpus-dir", "classifier-timeout"} {
		if !strings.Contains(help.String(), "-"+name+" ") {
			t.Fatalf("missing compatible flag %s: %s", name, help.String())
		}
	}
}
