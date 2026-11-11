package application

import (
	"errors"
	"reflect"
	"testing"
)

func TestResourcesCloseInReverseOrderPreservesErrorsAndRunsOnce(t *testing.T) {
	var acquired resources
	var closed []string
	storeErr, telemetryErr := errors.New("store close failed"), errors.New("telemetry flush failed")
	acquired.add(func() error { closed = append(closed, "telemetry"); return telemetryErr })
	acquired.add(func() error { closed = append(closed, "store"); return storeErr })
	acquired.release(func() { closed = append(closed, "worker") })
	for attempt := 0; attempt < 2; attempt++ {
		err := acquired.close()
		if !errors.Is(err, storeErr) || !errors.Is(err, telemetryErr) {
			t.Fatalf("Close lost cleanup errors: %v", err)
		}
	}
	if want := []string{"worker", "store", "telemetry"}; !reflect.DeepEqual(closed, want) {
		t.Fatalf("close order = %v, want %v exactly once", closed, want)
	}
}
