package tool

import (
	"context"
	"testing"
)

func TestAddToolExecute(t *testing.T) {
	tests := []struct {
		name    string
		args    string
		want    string
		wantErr bool
	}{
		{name: "add", args: `{"a":2,"b":3}`, want: "5"},
		{name: "zero is valid", args: `{"a":0,"b":3}`, want: "3"},
		{name: "missing parameter", args: `{"a":2}`, wantErr: true},
		{name: "unknown parameter", args: `{"a":2,"b":3,"other":1}`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := (AddTool{}).Execute(context.Background(), tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Execute(%q) error = %v, wantErr %v", tt.args, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("Execute(%q) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}
